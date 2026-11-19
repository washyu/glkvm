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
Guest agent API: talks to qemu-ga (or glkvm-agent, which speaks the same
protocol) running on the target host over the USB serial gadget, and exposes
host info (hostname, OS, IPs, users), clean shutdown/reboot and, with
glkvm-agent, clipboard sync.

  GET  /api/guest                         -- current state (cached)
  POST /api/guest/refresh                 -- re-read host info now
  POST /api/guest/power?action=shutdown   -- or action=reboot
  GET  /api/guest/clipboard               -- host clipboard text (cached)
  POST /api/guest/clipboard  {"text": ..} -- set the host clipboard

While someone reads /api/guest/clipboard, the host clipboard is polled every
second; otherwise the agent is just pinged every few seconds.

The USB serial port itself is switched on/off through the existing
/api/system/otg_functions?enable_serial=1 toggle.
"""


import os
import glob
import copy
import time
import asyncio

from typing import Any
from typing import AsyncGenerator

from aiohttp.web import Request
from aiohttp.web import Response

from .... import aiotools
from ....logging import get_logger
from ....validators.basic import valid_stripped_string_not_empty
from ....validators import check_string_in_list
from ....htserver import exposed_http
from ....htserver import make_json_response
from ....htserver import UnavailableError
from ....htserver import BadRequestError
from ....htserver import BadGatewayError

from ..guest_agent import QgaChannel
from ..guest_agent import QgaError
from ..guest_agent import QgaCommandError
from ..guest_agent import QgaChannelClosedError


# =====
_DEFAULT_DEVICE = "/dev/ttyGS0"
_ACM_FUNC = "acm.usb0"

# Info commands we try on each refresh, in order. Missing/disabled commands
# (older agents, blocked RPCs) are just skipped.
_INFO_COMMANDS = (
    "guest-get-host-name",
    "guest-get-osinfo",
    "guest-network-get-interfaces",
    "guest-get-users",
    "guest-get-timezone",
)

# glkvm-agent extensions (see contrib/glkvm-agent)
_CLIP_GET = "glkvm-clipboard-get"
_CLIP_SET = "glkvm-clipboard-set"
_CLIP_MAX_BYTES = 256 * 1024  # Same limit as the agent
_CLIP_WATCH_TIME = 30.0  # Keep polling fast this long after the last clipboard read

_POWER_MODES = {
    "shutdown": "powerdown",
    "reboot": "reboot",
}


def _is_port_linked() -> bool:
    # The gadget function exists whenever otg.devices.serial.enabled is set,
    # but the host only sees a COM port when it's linked into the config.
    return any(
        os.path.islink(path)
        for path in glob.glob(f"/sys/kernel/config/usb_gadget/*/configs/*/{_ACM_FUNC}")
    )


def _simplify_interfaces(raw: Any) -> list[dict]:
    result: list[dict] = []
    if not isinstance(raw, list):
        return result
    for iface in raw:
        if not isinstance(iface, dict):
            continue
        addrs = []
        for addr in (iface.get("ip-addresses") or []):
            if isinstance(addr, dict) and addr.get("ip-address"):
                addrs.append({
                    "type": str(addr.get("ip-address-type", "")),
                    "address": str(addr["ip-address"]),
                    "prefix": addr.get("prefix"),
                })
        result.append({
            "name": str(iface.get("name", "")),
            "mac": str(iface.get("hardware-address", "")),
            "addresses": addrs,
        })
    return result


def _simplify_users(raw: Any) -> list[dict]:
    if not isinstance(raw, list):
        return []
    return [
        {
            "user": str(user.get("user", "")),
            "domain": str(user.get("domain", "")),
            "login_time": user.get("login-time"),
        }
        for user in raw
        if isinstance(user, dict)
    ]


# =====
class GuestApi:
    def __init__(
        self,
        device_path: str=_DEFAULT_DEVICE,
        ping_interval: float=5.0,
        info_interval: float=60.0,
        command_timeout: float=5.0,
        clipboard_interval: float=1.0,
    ) -> None:

        self.__device_path = device_path
        self.__ping_interval = ping_interval
        self.__info_interval = info_interval
        self.__command_timeout = command_timeout
        self.__clipboard_interval = clipboard_interval

        self.__channel = QgaChannel(device_path)
        self.__notifier = aiotools.AioNotifier()
        self.__wakeup = asyncio.Event()

        self.__connected = False
        self.__last_seen: (float | None) = None
        self.__last_info_ts = 0.0
        self.__agent: (dict | None) = None
        self.__info: (dict | None) = None
        self.__error: (str | None) = None

        self.__clip_seq: (int | None) = None
        self.__clip_text: (str | None) = None
        self.__clip_truncated = False
        self.__clip_updated: (float | None) = None
        self.__clip_error: (str | None) = None
        self.__clip_watch_until = 0.0

    # ===== State

    async def get_state(self) -> dict:
        return {
            "port_enabled": _is_port_linked(),
            "device": os.path.exists(self.__device_path),
            "connected": self.__connected,
            "last_seen": self.__last_seen,
            "error": self.__error,
            "agent": copy.deepcopy(self.__agent),
            "info": copy.deepcopy(self.__info),
            "clipboard": {
                "supported": self.__clip_supported(),
                "seq": self.__clip_seq,
                "updated": self.__clip_updated,
                "length": (len(self.__clip_text) if self.__clip_text is not None else None),
                "error": self.__clip_error,
            },
        }

    async def trigger_state(self) -> None:
        self.__notifier.notify()

    async def poll_state(self) -> AsyncGenerator[dict, None]:
        prev: (dict | None) = None
        while True:
            state = await self.get_state()
            # last_seen ticks on every ping; don't broadcast just for that
            cmp = {key: value for (key, value) in state.items() if key != "last_seen"}
            if cmp != prev:
                prev = cmp
                yield state
            await self.__notifier.wait(timeout=5)

    # ===== System task

    async def systask(self) -> None:
        logger = get_logger(0)
        logger.info("Guest agent: using %s", self.__device_path)
        while True:
            try:
                await self.__run_session()
            except asyncio.CancelledError:
                raise
            except Exception as ex:
                logger.error("Guest agent session failed: %s", ex)
            finally:
                self.__channel.close()
                self.__set_connected(False, None)
            await self.__sleep(self.__ping_interval)

    async def __run_session(self) -> None:
        logger = get_logger(0)
        if not os.path.exists(self.__device_path):
            return  # Serial gadget function isn't configured; check again later

        self.__channel.open()
        while self.__channel.is_open():
            try:
                if not self.__channel.is_synced():
                    await self.__channel.sync(self.__command_timeout)
                    if not self.__connected:
                        logger.info("Guest agent: connected")
                    self.__set_connected(True, None)
                    await self.__refresh_info()
                else:
                    if self.__clip_watched():
                        await self.__poll_clipboard()  # Doubles as the ping
                    else:
                        await self.__channel.execute("guest-ping", timeout=self.__command_timeout)
                    self.__last_seen = time.time()
                    if time.time() - self.__last_info_ts >= self.__info_interval:
                        await self.__refresh_info()
            except QgaChannelClosedError:
                raise
            except QgaError as ex:
                if self.__connected:
                    logger.info("Guest agent: lost contact (%s)", ex)
                self.__set_connected(False, None)
            await self.__sleep(self.__clipboard_interval if self.__clip_watched() else self.__ping_interval)

    async def __sleep(self, delay: float) -> None:
        try:
            await asyncio.wait_for(self.__wakeup.wait(), timeout=delay)
        except asyncio.TimeoutError:
            pass
        self.__wakeup.clear()

    def __set_connected(self, connected: bool, error: (str | None)) -> None:
        if connected:
            self.__last_seen = time.time()
        if connected != self.__connected or error != self.__error:
            self.__connected = connected
            self.__error = error
            self.__notifier.notify()

    async def __refresh_info(self) -> None:
        agent_raw = await self.__channel.execute("guest-info", timeout=self.__command_timeout)
        if not isinstance(agent_raw, dict):
            agent_raw = {}
        listed = [
            cmd for cmd in (agent_raw.get("supported_commands") or [])
            if isinstance(cmd, dict) and cmd.get("name")
        ]
        enabled = {cmd["name"] for cmd in listed if cmd.get("enabled")}
        agent = {
            "version": str(agent_raw.get("version", "")),
            "commands": sorted(enabled),
        }

        results: dict[str, Any] = {}
        for command in _INFO_COMMANDS:
            # Skip only what the agent explicitly reports as disabled (e.g. not in
            # its --allow-rpcs list). If it sent no command list, just try.
            if listed and command not in enabled:
                continue
            try:
                results[command] = await self.__channel.execute(command, timeout=self.__command_timeout)
            except QgaCommandError as ex:
                get_logger(0).warning("Guest agent: %s failed: %s", command, ex)

        osinfo = results.get("guest-get-osinfo")
        info = {
            "hostname": str((results.get("guest-get-host-name") or {}).get("host-name", "")),
            "os": (osinfo if isinstance(osinfo, dict) else None),
            "interfaces": _simplify_interfaces(results.get("guest-network-get-interfaces")),
            "users": _simplify_users(results.get("guest-get-users")),
            "timezone": results.get("guest-get-timezone"),
            "updated": time.time(),
        }

        self.__last_info_ts = time.time()
        self.__last_seen = time.time()
        if agent != self.__agent or {**info, "updated": 0} != {**(self.__info or {}), "updated": 0}:
            self.__notifier.notify()
        self.__agent = agent
        self.__info = info

    # ===== Clipboard

    def __clip_supported(self) -> bool:
        return bool(self.__agent and _CLIP_GET in self.__agent["commands"])

    def __clip_watched(self) -> bool:
        return (self.__connected and self.__clip_supported() and time.time() < self.__clip_watch_until)

    async def __poll_clipboard(self) -> None:
        args = ({"since": self.__clip_seq} if self.__clip_seq is not None else None)
        try:
            result = await self.__channel.execute(_CLIP_GET, args, timeout=self.__command_timeout)
        except QgaCommandError as ex:
            # e.g. another app holds the clipboard; the agent itself is fine
            self.__set_clip_error(str(ex))
            return
        if not isinstance(result, dict) or not isinstance(result.get("seq"), int):
            self.__set_clip_error("Malformed clipboard reply")
            return
        self.__set_clip_error(None)
        if result.get("changed"):
            text = result.get("text")
            self.__clip_text = (text if isinstance(text, str) else None)
            self.__clip_truncated = bool(result.get("truncated"))
            self.__clip_updated = time.time()
            self.__notifier.notify()
        self.__clip_seq = result["seq"]

    def __set_clip_error(self, error: (str | None)) -> None:
        if error != self.__clip_error:
            if error:
                get_logger(0).warning("Guest agent: clipboard: %s", error)
            self.__clip_error = error
            self.__notifier.notify()

    # ===== HTTP

    @exposed_http("GET", "/guest")
    async def __state_handler(self, _: Request) -> Response:
        return make_json_response(await self.get_state())

    @exposed_http("POST", "/guest/refresh")
    async def __refresh_handler(self, _: Request) -> Response:
        self.__ensure_connected()
        try:
            await self.__refresh_info()
        except QgaError as ex:
            raise BadGatewayError(f"Guest agent error: {ex}")
        return make_json_response(await self.get_state())

    @exposed_http("POST", "/guest/power")
    async def __power_handler(self, req: Request) -> Response:
        action = check_string_in_list(
            valid_stripped_string_not_empty(req.query.get("action")),
            "Power action", list(_POWER_MODES),
        )
        self.__ensure_connected()
        try:
            await self.__channel.execute_no_reply(
                "guest-shutdown",
                {"mode": _POWER_MODES[action]},
                timeout=self.__command_timeout,
            )
        except QgaError as ex:
            raise BadGatewayError(f"Guest agent error: {ex}")
        get_logger(0).info("Guest agent: requested %s", action)
        self.__wakeup.set()  # Re-sync soon so the UI notices the host going away
        return make_json_response({"action": action})

    def __ensure_connected(self) -> None:
        if not self.__connected or not self.__channel.is_open():
            raise UnavailableError("Guest agent is not connected")

    @exposed_http("GET", "/guest/clipboard")
    async def __clipboard_get_handler(self, _: Request) -> Response:
        was_watched = self.__clip_watched()
        self.__clip_watch_until = time.time() + _CLIP_WATCH_TIME
        if not was_watched and self.__clip_watched():
            self.__wakeup.set()  # Start fast polling now, not after the current ping sleep
        return make_json_response({
            "supported": self.__clip_supported(),
            "connected": self.__connected,
            "seq": self.__clip_seq,
            "text": self.__clip_text,
            "truncated": self.__clip_truncated,
            "updated": self.__clip_updated,
            "error": self.__clip_error,
        })

    @exposed_http("POST", "/guest/clipboard")
    async def __clipboard_set_handler(self, req: Request) -> Response:
        try:
            body = await req.json()
        except Exception:
            raise BadRequestError("Expected a JSON body like {\"text\": \"...\"}")
        text = (body.get("text") if isinstance(body, dict) else None)
        if not isinstance(text, str):
            raise BadRequestError("Missing 'text'")
        if len(text.encode("utf-8", errors="replace")) > _CLIP_MAX_BYTES:
            raise BadRequestError(f"Text is too long (limit {_CLIP_MAX_BYTES // 1024} KiB)")
        if "\0" in text:
            raise BadRequestError("Text can't contain NUL characters")
        self.__ensure_connected()
        if not self.__clip_supported():
            raise UnavailableError("The guest agent has no clipboard support (needs glkvm-agent)")
        try:
            result = await self.__channel.execute(_CLIP_SET, {"text": text}, timeout=self.__command_timeout)
        except QgaError as ex:
            raise BadGatewayError(f"Guest agent error: {ex}")
        # Remember what we set, so the next poll doesn't report it as a host change
        if isinstance(result, dict) and isinstance(result.get("seq"), int):
            self.__clip_seq = result["seq"]
        self.__clip_text = text.replace("\r\n", "\n")
        self.__clip_truncated = False
        self.__clip_updated = time.time()
        self.__notifier.notify()
        return make_json_response({"seq": self.__clip_seq})
