# ========================================================================== #
#                                                                            #
#    KVMD - The main PiKVM daemon.                                           #
#                                                                            #
#    Copyright (C) 2018-2024  Maxim Devaev <mdevaev@gmail.com>               #
#                                                                            #
#    This program is free software: you can redistribute it and/or modify    #
#    it under the terms of the GNU General Public License as published by    #
#    the Free Software Foundation, either version 3 of the License, or       #
#    (at your option) any later version.                                     #
#                                                                            #
#    This program is distributed in the hope that it will be useful,         #
#    but WITHOUT ANY WARRANTY; without even the implied warranty of          #
#    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the           #
#    GNU General Public License for more details.                            #
#                                                                            #
#    You should have received a copy of the GNU General Public License       #
#    along with this program.  If not, see <https://www.gnu.org/licenses/>.  #
#                                                                            #
# ========================================================================== #


"""
Client for the QEMU Guest Agent (qemu-ga) protocol over a serial tty.

On the KVM the USB CDC-ACM gadget shows up as /dev/ttyGS0; the target host
sees it as a COM port (Windows) or /dev/ttyACM0 (Linux) and runs the stock
qemu-ga with "-m isa-serial -p <port>".

Protocol notes:
  * Requests and responses are JSON objects; qemu-ga ends each response
    with a newline, so we frame on newlines.
  * Sending a 0xFF byte resets qemu-ga's JSON parser. "guest-sync-delimited"
    makes the agent prefix its reply with 0xFF, which lets us throw away any
    stale output that piled up while nobody was listening.
  * Only one command is in flight at a time. After any timeout the channel
    is marked out of sync and must be re-synced before the next command,
    so a late reply can't be mistaken for the answer to a newer request.
"""


import os
import json
import tty
import termios
import random
import asyncio

from typing import Any


# =====
class QgaError(Exception):
    pass


class QgaTimeoutError(QgaError):
    pass


class QgaCommandError(QgaError):
    def __init__(self, cls: str, desc: str) -> None:
        super().__init__(f"{cls}: {desc}" if cls else desc)
        self.cls = cls
        self.desc = desc


class QgaChannelClosedError(QgaError):
    pass


# =====
_MAX_LINE = 1024 * 1024  # Sanity limit; real replies are a few KiB at most


class QgaChannel:
    def __init__(self, path: str) -> None:
        self.__path = path
        self.__fd = -1
        self.__buf = b""
        self.__lines: "asyncio.Queue[bytes]" = asyncio.Queue()
        self.__error: (Exception | None) = None
        self.__synced = False
        self.__lock = asyncio.Lock()

    # ===== Lifecycle

    def open(self) -> None:
        assert self.__fd < 0
        fd = os.open(self.__path, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
        try:
            if os.isatty(fd):
                # Raw mode: no echo (the agent would read its own replies back),
                # no CR/LF translation, no flow-control characters.
                tty.setraw(fd, termios.TCSANOW)
                termios.tcflush(fd, termios.TCIOFLUSH)
            asyncio.get_running_loop().add_reader(fd, self.__on_readable)
        except Exception:
            os.close(fd)
            raise
        self.__fd = fd
        self.__buf = b""
        self.__lines = asyncio.Queue()
        self.__error = None
        self.__synced = False

    def close(self) -> None:
        if self.__fd >= 0:
            fd = self.__fd
            self.__fd = -1
            self.__synced = False
            try:
                asyncio.get_running_loop().remove_reader(fd)
            except Exception:
                pass
            try:
                os.close(fd)
            except Exception:
                pass

    def is_open(self) -> bool:
        return (self.__fd >= 0)

    def is_synced(self) -> bool:
        return self.__synced

    # ===== Public API

    async def sync(self, timeout: float) -> None:
        async with self.__lock:
            await self.__sync(timeout)

    async def execute(
        self,
        command: str,
        arguments: (dict | None)=None,
        timeout: float=5.0,
    ) -> Any:

        async with self.__lock:
            if not self.__synced:
                await self.__sync(timeout)
            await self.__send(command, arguments, timeout)
            reply = await self.__read_reply(timeout)
            return self.__unwrap(reply)

    async def execute_no_reply(
        self,
        command: str,
        arguments: (dict | None)=None,
        timeout: float=5.0,
        error_wait: float=2.0,
    ) -> None:
        """
        For commands like guest-shutdown that send nothing back on success.
        We still wait briefly so an error reply (e.g. missing privilege) is
        reported instead of silently swallowed.
        """

        async with self.__lock:
            if not self.__synced:
                await self.__sync(timeout)
            await self.__send(command, arguments, timeout)
            try:
                reply = await self.__read_reply(error_wait)
            except QgaTimeoutError:
                return  # No news is good news
            finally:
                self.__synced = False  # The agent may be going away; resync next time
            self.__unwrap(reply)

    # ===== Internals

    async def __sync(self, timeout: float) -> None:
        self.__check_open()
        self.__synced = False
        self.__drain_queue()
        sync_id = random.randint(1, 2 ** 31 - 1)
        request = {"execute": "guest-sync-delimited", "arguments": {"id": sync_id}}
        await self.__write(b"\xFF" + json.dumps(request).encode() + b"\n", timeout)

        deadline = asyncio.get_running_loop().time() + timeout
        while True:
            remaining = deadline - asyncio.get_running_loop().time()
            if remaining <= 0:
                raise QgaTimeoutError("No reply to guest-sync-delimited")
            reply = await self.__read_reply(remaining, discard_garbage=True)
            if reply.get("return") == sync_id:
                self.__synced = True
                return
            # Anything else is stale output from before the sync; drop it

    async def __send(self, command: str, arguments: (dict | None), timeout: float) -> None:
        request: dict[str, Any] = {"execute": command}
        if arguments:
            request["arguments"] = arguments
        await self.__write(json.dumps(request).encode() + b"\n", timeout)

    async def __read_reply(self, timeout: float, discard_garbage: bool=False) -> dict:
        deadline = asyncio.get_running_loop().time() + timeout
        while True:
            remaining = deadline - asyncio.get_running_loop().time()
            if remaining <= 0:
                self.__synced = False
                raise QgaTimeoutError("Timed out waiting for the guest agent")
            try:
                line = await asyncio.wait_for(self.__lines.get(), timeout=remaining)
            except asyncio.TimeoutError:
                self.__synced = False
                raise QgaTimeoutError("Timed out waiting for the guest agent")
            if not line:
                # Empty marker pushed by the reader on error/EOF
                self.__synced = False
                raise QgaChannelClosedError(str(self.__error or "Channel closed"))

            text = line.lstrip(b"\xFF \t\r\n").rstrip()
            if not text:
                continue
            try:
                reply = json.loads(text)
            except ValueError:
                if discard_garbage:
                    continue
                self.__synced = False
                raise QgaError(f"Malformed reply from the guest agent: {text[:100]!r}")
            if not isinstance(reply, dict):
                if discard_garbage:
                    continue
                self.__synced = False
                raise QgaError("Unexpected reply type from the guest agent")
            return reply

    def __unwrap(self, reply: dict) -> Any:
        if "error" in reply:
            err = reply["error"] if isinstance(reply["error"], dict) else {}
            raise QgaCommandError(str(err.get("class", "")), str(err.get("desc", "Unknown error")))
        if "return" not in reply:
            self.__synced = False
            raise QgaError("Reply without 'return' from the guest agent")
        return reply["return"]

    async def __write(self, data: bytes, timeout: float) -> None:
        self.__check_open()
        loop = asyncio.get_running_loop()
        deadline = loop.time() + timeout
        while data:
            try:
                written = os.write(self.__fd, data)
                data = data[written:]
                continue
            except BlockingIOError:
                pass
            except OSError as ex:
                raise QgaChannelClosedError(f"Write failed: {ex}")

            # The gadget buffer is full (usually nobody has the port open on the host)
            remaining = deadline - loop.time()
            if remaining <= 0:
                self.__synced = False
                raise QgaTimeoutError("Timed out writing to the guest agent port")
            writable = loop.create_future()
            loop.add_writer(self.__fd, (lambda: writable.done() or writable.set_result(None)))
            try:
                await asyncio.wait_for(writable, timeout=remaining)
            except asyncio.TimeoutError:
                pass
            finally:
                loop.remove_writer(self.__fd)

    def __on_readable(self) -> None:
        try:
            data = os.read(self.__fd, 4096)
        except BlockingIOError:
            return
        except OSError as ex:
            data = b""
            self.__error = ex
        if not data:
            # EOF or error (e.g. the gadget was torn down): stop watching the fd
            # and wake any reader up with an empty marker.
            if self.__error is None:
                self.__error = QgaChannelClosedError("EOF on the guest agent port")
            try:
                asyncio.get_running_loop().remove_reader(self.__fd)
            except Exception:
                pass
            self.__lines.put_nowait(b"")
            return

        self.__buf += data
        while True:
            (line, sep, rest) = self.__buf.partition(b"\n")
            if not sep:
                break
            self.__buf = rest
            self.__lines.put_nowait(line)
        if len(self.__buf) > _MAX_LINE:
            self.__buf = b""  # Garbage without newlines; drop it

    def __drain_queue(self) -> None:
        while not self.__lines.empty():
            line = self.__lines.get_nowait()
            if not line:
                # Keep the closed marker visible to the next reader
                self.__lines.put_nowait(line)
                return

    def __check_open(self) -> None:
        if self.__fd < 0:
            raise QgaChannelClosedError("Guest agent port is not open")
        if self.__error is not None:
            raise QgaChannelClosedError(str(self.__error))
