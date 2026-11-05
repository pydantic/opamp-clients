# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Windows' side of the client's files, through the Win32 API (ctypes): what os.open's mode and
os.replace don't do there.

- The user's only. A mode of 0600 or 0700 means nothing on Windows: a new state file, download,
  downloads directory or core tap is made with an access-control list that allows the user the
  process runs as everything and nobody else anything, inheriting nothing (a directory's is
  inherited by what is made in it), as the .NET and Java clients make them.
- A rename that reaches the disk. A directory can't be flushed on Windows; MoveFileEx with
  MOVEFILE_WRITE_THROUGH returns only once the rename has been written out."""

from __future__ import annotations

import ctypes
import errno
import os
from collections.abc import Callable
from typing import Any

MOVEFILE_REPLACE_EXISTING = 0x1
MOVEFILE_WRITE_THROUGH = 0x8

_GENERIC_WRITE = 0x40000000
_FILE_APPEND_DATA = 0x4
_SYNCHRONIZE = 0x100000
_READ_CONTROL = 0x20000
_FILE_SHARE_READ = 0x1
_FILE_SHARE_WRITE = 0x2
_CREATE_NEW = 1
_OPEN_ALWAYS = 4
_FILE_ATTRIBUTE_NORMAL = 0x80
_INVALID_HANDLE = ctypes.c_void_p(-1).value
_TOKEN_QUERY = 0x8
_TOKEN_USER = 1
_ERROR_ALREADY_EXISTS = 183
_SDDL_REVISION_1 = 1
_DACL_SECURITY_INFORMATION = 0x4

_k32: Any = None
_adv: Any = None


def _dlls() -> tuple[Any, Any]:
    global _k32, _adv
    if _k32 is None:
        from ctypes import wintypes as w

        k32 = ctypes.WinDLL("kernel32", use_last_error=True)
        adv = ctypes.WinDLL("advapi32", use_last_error=True)
        k32.CreateFileW.argtypes = [w.LPCWSTR, w.DWORD, w.DWORD, ctypes.c_void_p, w.DWORD, w.DWORD, w.HANDLE]
        k32.CreateFileW.restype = w.HANDLE
        k32.CreateDirectoryW.argtypes = [w.LPCWSTR, ctypes.c_void_p]
        k32.CreateDirectoryW.restype = w.BOOL
        k32.MoveFileExW.argtypes = [w.LPCWSTR, w.LPCWSTR, w.DWORD]
        k32.MoveFileExW.restype = w.BOOL
        k32.CloseHandle.argtypes = [w.HANDLE]
        k32.GetCurrentProcess.restype = w.HANDLE
        k32.LocalFree.argtypes = [ctypes.c_void_p]
        adv.OpenProcessToken.argtypes = [w.HANDLE, w.DWORD, ctypes.POINTER(w.HANDLE)]
        adv.GetTokenInformation.argtypes = [w.HANDLE, ctypes.c_int, ctypes.c_void_p, w.DWORD, ctypes.POINTER(w.DWORD)]
        adv.ConvertSidToStringSidW.argtypes = [ctypes.c_void_p, ctypes.POINTER(w.LPWSTR)]
        adv.ConvertStringSecurityDescriptorToSecurityDescriptorW.argtypes = [
            w.LPCWSTR, w.DWORD, ctypes.POINTER(ctypes.c_void_p), ctypes.POINTER(w.ULONG)]
        adv.GetFileSecurityW.argtypes = [w.LPCWSTR, w.DWORD, ctypes.c_void_p, w.DWORD, ctypes.POINTER(w.DWORD)]
        adv.ConvertSecurityDescriptorToStringSecurityDescriptorW.argtypes = [
            ctypes.c_void_p, w.DWORD, w.DWORD, ctypes.POINTER(w.LPWSTR), ctypes.POINTER(w.ULONG)]
        _k32, _adv = k32, adv
    return _k32, _adv


def _fail(what: str, path: str | None = None) -> OSError:
    """The OSError for the Win32 call that just failed (one of a stand-in's, off Windows: EIO)."""
    if not hasattr(ctypes, "WinError"):
        return OSError(errno.EIO, f"{what} failed", path)
    code = ctypes.get_last_error()
    e = ctypes.WinError(code)
    return OSError(e.errno, f"{what}: {e.strerror}", path, code)


_user: str | None = None


def user_sid() -> str:
    """The SID of the user the process runs as (S-1-5-...)."""
    global _user
    if _user is None:
        from ctypes import wintypes as w

        k32, adv = _dlls()
        token = w.HANDLE()
        if not adv.OpenProcessToken(k32.GetCurrentProcess(), _TOKEN_QUERY, ctypes.byref(token)):
            raise _fail("OpenProcessToken")
        try:
            size = w.DWORD()
            adv.GetTokenInformation(token, _TOKEN_USER, None, 0, ctypes.byref(size))
            buf = ctypes.create_string_buffer(size.value)
            if not adv.GetTokenInformation(token, _TOKEN_USER, buf, size, ctypes.byref(size)):
                raise _fail("GetTokenInformation")
            sid = ctypes.c_void_p.from_buffer(buf).value  # TOKEN_USER.User.Sid
            text = w.LPWSTR()
            if not adv.ConvertSidToStringSidW(sid, ctypes.byref(text)):
                raise _fail("ConvertSidToStringSidW")
            try:
                _user = text.value
            finally:
                k32.LocalFree(text)
        finally:
            k32.CloseHandle(token)
    assert _user is not None
    return _user


def user_only(directory: bool = False) -> str:
    """The access-control list, as SDDL: the user may do anything, nobody else anything, nothing
    inherited (protected); a directory's is inherited by its files and directories."""
    return f"D:P(A;{'OICI' if directory else ''};FA;;;{user_sid()})"


class _Attributes(ctypes.Structure):
    _fields_ = [("nLength", ctypes.c_ulong), ("lpSecurityDescriptor", ctypes.c_void_p), ("bInheritHandle", ctypes.c_int)]


def _with_list(directory: bool, make: Callable[[ctypes.c_void_p], Any]) -> Any:
    return _with_sd(user_only(directory), lambda sd: make(ctypes.byref(_Attributes(ctypes.sizeof(_Attributes), sd.value, 0))))


def create_private(path: str, append: bool = False) -> int:
    """A file descriptor open for writing at `path`, the user's only: made new (an error if one is
    there), or with `append`, opened for appending, made if need be (one already there keeps its
    list)."""
    import msvcrt

    k32, _ = _dlls()
    access = (_FILE_APPEND_DATA | _SYNCHRONIZE | _READ_CONTROL) if append else _GENERIC_WRITE
    disposition = _OPEN_ALWAYS if append else _CREATE_NEW

    def make(attrs: Any) -> Any:
        return k32.CreateFileW(path, access, _FILE_SHARE_READ | _FILE_SHARE_WRITE, attrs, disposition, _FILE_ATTRIBUTE_NORMAL, None)

    handle = _with_list(False, make)
    if handle is None or handle == _INVALID_HANDLE:
        raise _fail("CreateFileW", path)
    try:
        return msvcrt.open_osfhandle(handle, os.O_WRONLY | os.O_BINARY | (os.O_APPEND if append else 0))
    except BaseException:
        k32.CloseHandle(handle)
        raise


def make_private_dir(path: str) -> bool:
    """Make directory `path` (its parents as need be), the user's only, unless it is there: whether
    it was made."""
    if os.path.isdir(path):
        return False
    parent = os.path.dirname(os.path.abspath(path))
    os.makedirs(parent, exist_ok=True)
    k32, _ = _dlls()
    if _with_list(True, lambda attrs: k32.CreateDirectoryW(path, attrs)):
        return True
    if ctypes.get_last_error() == _ERROR_ALREADY_EXISTS and os.path.isdir(path):
        return False
    raise _fail("CreateDirectoryW", path)


def replace(src: str, dst: str, move: Callable[[str, str, int], bool] | None = None) -> None:
    """Rename `src` over `dst`, returning once the rename is on the disk (MOVEFILE_WRITE_THROUGH):
    through `move` (MoveFileExW by default; a stand-in in tests)."""
    if move is None:
        move = _dlls()[0].MoveFileExW
    if not move(src, dst, MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH):
        raise _fail("MoveFileExW", src)


def dacl(path: str) -> str:
    """`path`'s access-control list, as SDDL (for the tests)."""
    from ctypes import wintypes as w

    _, adv = _dlls()
    size = w.DWORD()
    adv.GetFileSecurityW(path, _DACL_SECURITY_INFORMATION, None, 0, ctypes.byref(size))
    buf = ctypes.create_string_buffer(size.value)
    if not adv.GetFileSecurityW(path, _DACL_SECURITY_INFORMATION, buf, size, ctypes.byref(size)):
        raise _fail("GetFileSecurityW", path)
    return _sddl(buf)


def canonical(sddl: str) -> str:
    """`sddl` as Windows writes it back (a well-known SID by its alias, as LA for the local
    Administrator): what `dacl` returns for a file with that list (for the tests)."""
    return _with_sd(sddl, _sddl)


def _with_sd(sddl: str, use: Callable[[Any], Any]) -> Any:
    k32, adv = _dlls()
    sd = ctypes.c_void_p()
    if not adv.ConvertStringSecurityDescriptorToSecurityDescriptorW(sddl, _SDDL_REVISION_1, ctypes.byref(sd), None):
        raise _fail("ConvertStringSecurityDescriptorToSecurityDescriptorW")
    try:
        return use(sd)
    finally:
        k32.LocalFree(sd)


def _sddl(sd: Any) -> str:
    from ctypes import wintypes as w

    k32, adv = _dlls()
    text = w.LPWSTR()
    if not adv.ConvertSecurityDescriptorToStringSecurityDescriptorW(sd, _SDDL_REVISION_1, _DACL_SECURITY_INFORMATION, ctypes.byref(text), None):
        raise _fail("ConvertSecurityDescriptorToStringSecurityDescriptorW")
    try:
        return text.value or ""
    finally:
        k32.LocalFree(text)
