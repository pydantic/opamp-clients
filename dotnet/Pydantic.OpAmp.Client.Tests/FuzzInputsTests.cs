// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Pydantic.OpAmp.Client.Fuzz;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

// The SharpFuzz targets (dotnet/Pydantic.OpAmp.Client.Fuzz/Targets, compiled in here too) on their
// seeds and on every input kept under that project's crashes/<target>/: what a fuzzer found once is
// run with every dotnet test (scripts/dotnet_fuzz.sh runs the targets under libFuzzer).
public class FuzzInputsTests
{
    public static TheoryData<string, string> Inputs()
    {
        var data = new TheoryData<string, string>();
        foreach (var (target, seeds) in new[] { ("ClientFuzz", ClientFuzz.Seeds), ("CoreFuzz", CoreFuzz.Seeds) })
        {
            for (var i = 0; i < seeds.Length; i++)
            {
                data.Add(target, $"seed-{i}");
            }

            var dir = Path.Combine(AppContext.BaseDirectory, "fuzz-crashes", target);
            if (Directory.Exists(dir))
            {
                foreach (var f in Directory.GetFiles(dir).Order(StringComparer.Ordinal))
                {
                    data.Add(target, Path.GetFileName(f));
                }
            }
        }

        return data;
    }

    [Theory]
    [MemberData(nameof(Inputs))]
    public void TheTargetHoldsOn(string target, string input)
    {
        var seeds = target == "ClientFuzz" ? ClientFuzz.Seeds : CoreFuzz.Seeds;
        var data = input.StartsWith("seed-", StringComparison.Ordinal)
            ? seeds[int.Parse(input["seed-".Length..], System.Globalization.CultureInfo.InvariantCulture)]
            : File.ReadAllBytes(Path.Combine(AppContext.BaseDirectory, "fuzz-crashes", target, input));
        if (target == "ClientFuzz")
        {
            ClientFuzz.Run(data);
        }
        else
        {
            CoreFuzz.Run(data);
        }
    }
}
