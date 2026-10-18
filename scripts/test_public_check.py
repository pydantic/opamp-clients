#!/usr/bin/env python3
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""scripts/public_check.py's rules."""
from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import public_check as pc  # noqa: E402

ALLOW = pc.patterns("# a comment\npython/\n.github/workflows/*.yml\nREADME.md  # the front page\n")


class PublicCheck(unittest.TestCase):
    def test_the_allowlist(self) -> None:
        self.assertEqual(ALLOW, ["python/", ".github/workflows/*.yml", "README.md"])
        for rel in ["python/a.py", "python/deep/b.py", ".github/workflows/ci.yml", "README.md"]:
            self.assertEqual(pc.problems(rel, b"x", ALLOW), [], rel)
        for rel in ["pythonx/a.py", ".github/workflows/sub/ci.yml", "docs/README.md", "README.md.bak"]:
            self.assertEqual(pc.problems(rel, b"x", ALLOW), [f"{rel}: not in PUBLIC_ALLOWLIST"], rel)

    def test_a_binary(self) -> None:
        for magic in [b"\x7fELF", b"\xcf\xfa\xed\xfe", b"MZ", b"\0asm"]:
            self.assertEqual(pc.problems("python/x", magic + b"rest", ALLOW), ["python/x: a binary"])

    def test_a_module_where_the_clients_load_it(self) -> None:
        rel = "python/src/pydantic_opamp_client/opamp_core.wasm"
        self.assertIn(rel, pc.MODULE_PATHS)
        self.assertEqual(pc.problems(rel, b"\0asm", ALLOW), [])

    def test_a_credential(self) -> None:
        token = b"ghp_" + b"a" * 36
        self.assertEqual(pc.problems("python/a.py", b"t = '" + token + b"'", ALLOW),
                         ["python/a.py: holds a credential or private key"])

    def test_a_private_key_only_as_a_test_fixture(self) -> None:
        key = b"-----BEGIN EC " + b"PRIVATE KEY-----\n"  # split, so this file holds no key
        self.assertEqual(pc.problems("python/tests/tls/k.pem", key, ALLOW), [])
        self.assertEqual(pc.problems("python/k.pem", key, ALLOW), ["python/k.pem: holds a credential or private key"])

    def test_this_repository(self) -> None:
        self.assertEqual(pc.main(), 0)


if __name__ == "__main__":
    unittest.main()
