"""
TEST-ONLY hook for trying the guest agent on a device whose firmware is newer
than this repo (the device ships compiled .pyc files we must not overwrite).

Installed next to a one-line glkvm_guest_hook.pth in site-packages. When any
process imports kvmd.apps.kvmd.server, this wraps KvmdServer.__init__ so the
GuestApi is added to the server's APIs and subsystems, exactly like the
server.py change in this repo does. Every other process is unaffected.

Remove both files to undo it. Not needed once server.py includes GuestApi.
"""

import sys
import importlib.abc
import importlib.machinery

_TARGET = "kvmd.apps.kvmd.server"


def _patch(module):  # type: ignore
    try:
        from kvmd.apps.kvmd.api.guest import GuestApi
        server_cls = module.KvmdServer
        subsystem_cls = module._Subsystem  # pylint: disable=protected-access
        if getattr(server_cls, "_guest_hooked", False):
            return
        orig_init = server_cls.__init__

        def __init__(self, *args, **kwargs):  # type: ignore
            orig_init(self, *args, **kwargs)
            api = GuestApi()
            self._KvmdServer__apis.append(api)  # pylint: disable=protected-access
            self._KvmdServer__subsystems.append(  # pylint: disable=protected-access
                subsystem_cls.make(api, "Guest agent", "guest"),
            )

        server_cls.__init__ = __init__
        server_cls._guest_hooked = True

        # Same as the repo's system.py change: expose the serial port in the
        # existing OTG function toggle (/api/system/otg_functions?enable_serial=1)
        from kvmd.apps.kvmd.api.system import SystemApi
        func_map = getattr(SystemApi, "_OTG_FUNC_MAP", None)
        if isinstance(func_map, dict):
            func_map.setdefault("enable_serial", ("otg/devices/serial/start", False, "acm.usb0"))
    except Exception as ex:  # Never break kvmd because of the hook
        print(f"glkvm_guest_hook: failed to patch kvmd: {ex!r}", file=sys.stderr)


class _Loader(importlib.abc.Loader):
    def __init__(self, inner):  # type: ignore
        self.__inner = inner

    def create_module(self, spec):  # type: ignore
        return self.__inner.create_module(spec)

    def exec_module(self, module):  # type: ignore
        self.__inner.exec_module(module)
        _patch(module)


class _Finder(importlib.abc.MetaPathFinder):
    def find_spec(self, fullname, path, target=None):  # type: ignore
        if fullname != _TARGET:
            return None
        spec = importlib.machinery.PathFinder.find_spec(fullname, path)
        if spec is not None and spec.loader is not None:
            spec.loader = _Loader(spec.loader)
        return spec


if not any(isinstance(finder, _Finder) for finder in sys.meta_path):
    sys.meta_path.insert(0, _Finder())
