// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Pydantic.OpAmp.Client.Fuzz;

// Program TARGET: run TARGET (ClientFuzz, CoreFuzz) under libfuzzer-dotnet (scripts/dotnet_fuzz.sh).
// Program seeds TARGET DIR: write TARGET's seeds into DIR (a corpus to start from).
// Program replay TARGET FILE...: run TARGET on each FILE (a crash, to see it again).
var targets = new Dictionary<string, (SharpFuzz.ReadOnlySpanAction Run, byte[][] Seeds)>
{
    ["ClientFuzz"] = (ClientFuzz.Run, ClientFuzz.Seeds),
    ["CoreFuzz"] = (CoreFuzz.Run, CoreFuzz.Seeds),
};

switch (args)
{
    case [var name] when targets.TryGetValue(name, out var t):
        SharpFuzz.Fuzzer.LibFuzzer.Run(t.Run);
        return 0;
    case ["seeds", var name, var dir] when targets.TryGetValue(name, out var t):
        Directory.CreateDirectory(dir);
        for (var i = 0; i < t.Seeds.Length; i++)
        {
            File.WriteAllBytes(Path.Combine(dir, $"seed-{i}"), t.Seeds[i]);
        }

        return 0;
    case ["replay", var name, .. var files] when targets.TryGetValue(name, out var t):
        foreach (var f in files)
        {
            Console.WriteLine($"== {f}");
            t.Run(File.ReadAllBytes(f));
        }

        return 0;
    default:
        Console.Error.WriteLine($"usage: TARGET | seeds TARGET DIR | replay TARGET FILE... (TARGET: {string.Join(", ", targets.Keys)})");
        return 2;
}
