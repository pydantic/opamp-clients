// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Text.Json;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Xunit;

namespace Pydantic.OpAmp.Extensions.Hosting.Tests;

/// <summary>
/// The off switch (tests/shell-tables/off.json): every case through WithOpAmp, the case's variables
/// in the configuration (where the package reads them first) and the code's endpoint set by
/// WithOpAmp's delegate. Off: no client starts and the reason is said once, at Warning; on: the
/// client starts and the server hears from it. Every endpoint of a case is the test server's.
/// </summary>
public class OffSwitchTests
{
    private static CancellationToken Cancel => TestContext.Current.CancellationToken;

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

    internal static JsonElement Table()
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
    public async Task TheOffSwitchTable(string id)
    {
        var c = Table().GetProperty("cases").EnumerateArray().Single(e => e.GetProperty("id").GetString() == id);
        await using var server = new OpAmpServer();
        string Local(string v) => v.StartsWith("http://", StringComparison.Ordinal) || v.StartsWith("https://", StringComparison.Ordinal) ? server.Endpoint : v;
        var home = Kit.TempDir();
        var config = new Dictionary<string, string?>
        {
            ["OTEL_OPAMP_STATE_DIR"] = "none", ["XDG_STATE_HOME"] = home, ["HOME"] = home, ["LOCALAPPDATA"] = home,
        };
        foreach (var v in c.GetProperty("env").EnumerateObject())
        {
            config[v.Name] = Local(v.Value.GetString()!);
        }

        var code = c.GetProperty("code_endpoint") is { ValueKind: JsonValueKind.String } e ? new Uri(Local(e.GetString()!)) : null;
        var off = c.GetProperty("off").GetBoolean();
        var reason = c.GetProperty("reason").GetString()!;

        var said = new Said();
        var b = Host.CreateApplicationBuilder(new HostApplicationBuilderSettings { DisableDefaults = true });
        b.Configuration.AddInMemoryCollection(config);
        b.Logging.AddProvider(said);
        b.Services.AddOpenTelemetry().WithOpAmp(o =>
        {
            if (code is not null)
            {
                o.Endpoint = code;
            }
        });
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>().Agent;
        if (off)
        {
            Assert.Null(agent);
        }
        else
        {
            Assert.NotNull(agent);
            await Kit.Until(() => !server.Messages.IsEmpty, "the first message");
        }

        await host.StopAsync(Cancel);
        if (off)
        {
            Assert.Empty(server.Messages);
        }

        var lines = said.Lines.Where(l => l.Contains("OpAMP: off", StringComparison.Ordinal)).ToList();
        Assert.Equal(reason.Length > 0 ? [$"Warning: OpAMP: {reason}"] : [], lines);
    }

    private sealed class Said : ILoggerProvider, ILogger
    {
        public System.Collections.Concurrent.ConcurrentQueue<string> Lines { get; } = new();

        public ILogger CreateLogger(string categoryName) => this;

        public IDisposable? BeginScope<TState>(TState state)
            where TState : notnull => null;

        public bool IsEnabled(LogLevel logLevel) => true;

        public void Log<TState>(LogLevel logLevel, EventId eventId, TState state, Exception? exception, Func<TState, Exception?, string> formatter) =>
            this.Lines.Enqueue($"{logLevel}: {formatter(state, exception)}");

        public void Dispose()
        {
        }
    }
}
