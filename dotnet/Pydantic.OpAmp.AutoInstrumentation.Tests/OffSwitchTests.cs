// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Text.Json;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

[CollectionDefinition(nameof(StandardError), DisableParallelization = true)]
public sealed class StandardError
{
}

/// <summary>
/// The off switch (tests/shell-tables/off.json): every case through the plugin's start (its
/// Prepare, given the case's variables). The plugin has no code of its own: the code's endpoint is
/// OTEL_OPAMP_ENDPOINT. Off: no agent, so no client, and the reason is said once on standard
/// error; on: an agent.
/// </summary>
[Collection(nameof(StandardError))]
public class OffSwitchTests
{
    public static TheoryData<string> Cases()
    {
        var data = new TheoryData<string>();
        foreach (var c in Table().GetProperty("cases").EnumerateArray())
        {
            data.Add(c.GetProperty("id").GetString()!);
        }

        Assert.Equal(16, data.Count);
        return data;
    }

    private static JsonElement Table()
    {
        for (var dir = new DirectoryInfo(AppContext.BaseDirectory); dir is not null; dir = dir.Parent)
        {
            var file = Path.Combine(dir.FullName, "tests", "shell-tables", "off.json");
            if (File.Exists(file))
            {
                return JsonDocument.Parse(File.ReadAllText(file)).RootElement;
            }
        }

        throw new FileNotFoundException($"no tests/shell-tables/off.json above {AppContext.BaseDirectory}");
    }

    [Theory]
    [MemberData(nameof(Cases))]
    public void TheOffSwitchTable(string id)
    {
        var c = Table().GetProperty("cases").EnumerateArray().Single(e => e.GetProperty("id").GetString() == id);
        var env = new Dictionary<string, string?> { ["OTEL_OPAMP_STATE_DIR"] = "none" };
        foreach (var v in c.GetProperty("env").EnumerateObject())
        {
            env[v.Name] = v.Value.GetString();
        }

        if (c.GetProperty("code_endpoint") is { ValueKind: JsonValueKind.String } code)
        {
            env["OTEL_OPAMP_ENDPOINT"] = code.GetString();
        }

        var off = c.GetProperty("off").GetBoolean();
        var reason = c.GetProperty("reason").GetString()!;
        var was = Console.Error;
        var err = new StringWriter();
        Agent? agent;
        Console.SetError(err);
        try
        {
            agent = Agent.Prepare(n => env.GetValueOrDefault(n), (_, _) => { });
        }
        finally
        {
            Console.SetError(was);
        }

        try
        {
            Assert.Equal(off, agent is null);
            var lines = err.ToString().Split('\n').Select(l => l.TrimEnd('\r')).Where(l => l.Contains("OpAMP: off", StringComparison.Ordinal)).ToList();
            Assert.Equal(reason.Length > 0 ? [$"OpAMP: {reason}"] : [], lines);
        }
        finally
        {
            agent?.Stop(TimeSpan.FromSeconds(1));
        }
    }
}
