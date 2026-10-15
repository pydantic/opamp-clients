// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Protobuf strings, decoded exactly. @bufbuild/protobuf (2.15 and 2.16 alike) decodes every string
// field with a default `new TextDecoder()`, which drops a leading U+FEFF: a pattern, header value
// or name a server sends as "﻿x" reached this client as "x", where every other language's
// client keeps it (the settings' random vectors found it). Imported, for its effect, by each module
// that decodes: from then on the package decodes with `ignoreBOM: true`, as the protobuf encoding
// says a string is its bytes. The setting is the protobuf package's, so another user of the same
// copy in the process decodes exactly too; it changes nothing else of theirs.

import { configureTextEncoding, getTextEncoding } from "@bufbuild/protobuf/wire";

const base = getTextEncoding();
const exact = new TextDecoder("utf-8", { ignoreBOM: true });
const exactStrict = new TextDecoder("utf-8", { ignoreBOM: true, fatal: true });

configureTextEncoding({
  ...base,
  decodeUtf8: (bytes: Uint8Array, strict?: boolean) => (strict ? exactStrict : exact).decode(bytes),
});
