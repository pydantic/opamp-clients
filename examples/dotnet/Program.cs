// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using System.Text.Json;
using Google.Protobuf;
using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Resources;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client;

string[] identifying = ["service.name", "service.namespace", "service.instance.id"];

// One UUID is both the OpAMP instance UID and service.instance.id, so the fleet view and the
// telemetry name the same instance.
var uid = Guid.NewGuid();
var resourceBuilder = ResourceBuilder.CreateDefault()
    .AddService("checkout", serviceInstanceId: uid.ToString(), autoGenerateServiceInstanceId: false);
var resource = resourceBuilder.Build();
var sampler = new LiveSampler(1.0);
var source = new ActivitySource("checkout");

// Spans go wherever the server says (telemetry settings). An exporter gets the resource from its
// provider, so a new place means a new provider: build it, then dispose of the old one (which
// flushes what it holds).
TracerProvider Build(BaseExporter<Activity>? exporter)
{
    var builder = Sdk.CreateTracerProviderBuilder()
        .SetResourceBuilder(resourceBuilder)
        .SetSampler(new ParentBasedSampler(sampler))
        .AddSource(source.Name);
    if (exporter is not null)
    {
        builder.AddProcessor(new BatchActivityExportProcessor(exporter, scheduledDelayMilliseconds: 500));
    }

    return builder.Build()!;
}

var tracerProvider = Build(null);

var description = new AgentDescription();
foreach (var (key, value) in resource.Attributes)
{
    var kv = new KeyValue { Key = key, Value = new AnyValue { StringValue = Convert.ToString(value) ?? string.Empty } };
    (identifying.Contains(key) ? description.IdentifyingAttributes : description.NonIdentifyingAttributes).Add(kv);
}

// Notifications arrive on the client's loop, with the client to answer through (a remote config
// saved before a restart comes as the client starts).
var client = OpAmpClient.Start(
    new OpAmpClientOptions
    {
        Endpoint = new Uri(Environment.GetEnvironmentVariable("OPAMP_ENDPOINT")!), // https://.../v1/opamp, or wss://...
        InstanceUid = uid.ToByteArray(bigEndian: true),
        Capabilities = (ulong)(AgentCapabilities.AcceptsRemoteConfig | AgentCapabilities.ReportsOwnTraces),
        // Headers = new Dictionary<string, string> { ["Authorization"] = $"Bearer {token}" },
    },
    new OpAmpStatus
    {
        Description = description,
        Health = new ComponentHealth { Healthy = true },
        EffectiveConfig = Effective(1.0),
    },
    (c, n) =>
    {
        if (n is OpAmpNotification.Stopped { Error: { } error })
        {
            // The client stopped on its own (a bug): don't carry on unmanaged.
            Console.Error.WriteLine($"OpAMP client stopped: {error}");
            Environment.Exit(1);
        }

        if (n is OpAmpNotification.ConnectionSettings cs)
        {
            // Where to send our own telemetry: all the settings, not only what changed.
            var answer = new ConnectionSettingsStatus
            {
                LastConnectionSettingsHash = cs.Settings.Hash,
                Status = ConnectionSettingsStatuses.Applied,
            };
            if (cs.Settings.OwnTraces is { } traces)
            {
                try
                {
                    Interlocked.Exchange(ref tracerProvider, Build(Exporter(traces))).Dispose();
                }
                catch (Exception e)
                {
                    answer.Status = ConnectionSettingsStatuses.Failed;
                    answer.ErrorMessage = e.Message;
                }
            }

            c.SetConnectionSettingsStatus(answer);
            return;
        }

        if (n is not OpAmpNotification.RemoteConfig rc)
        {
            return;
        }

        var status = new RemoteConfigStatus { LastRemoteConfigHash = rc.Config.ConfigHash };
        if (SamplingRatio(rc.Config) is { } ratio)
        {
            sampler.Current = new TraceIdRatioBasedSampler(ratio);
            status.Status = RemoteConfigStatuses.Applied;
            c.SetEffectiveConfig(Effective(ratio));
        }
        else
        {
            status.Status = RemoteConfigStatuses.Failed;
            status.ErrorMessage = """want {"sampling_ratio": 0..1}""";
        }

        c.SetRemoteConfigStatus(status);
    });

// The application's work: a span every 100 ms.
using var work = new Timer(_ => source.StartActivity("checkout")?.Dispose(), null, 0, 100);

// The program runs until interrupted. Then the server hears the agent go (agent_disconnect), and
// the SDK flushes.
var stop = new TaskCompletionSource();
Console.CancelKeyPress += (_, e) =>
{
    e.Cancel = true;
    stop.TrySetResult();
};
await stop.Task;
await client.StopAsync();
tracerProvider.Dispose();

// An OTLP/HTTP exporter for the traces part of telemetry settings: the endpoint is the full URL
// (.../v1/traces), with the headers offered.
static OtlpTraceExporter Exporter(TelemetryConnectionSettings t) => new(new OtlpExporterOptions
{
    Endpoint = new Uri(t.DestinationEndpoint),
    Protocol = OtlpExportProtocol.HttpProtobuf,
    Headers = string.Join(",", t.Headers?.Headers_.Select(h => $"{h.Key}={h.Value}") ?? []),
});

static EffectiveConfig Effective(double ratio)
{
    var file = new AgentConfigObject
    {
        Body = ByteString.CopyFromUtf8(JsonSerializer.Serialize(new { sampling_ratio = ratio })),
        ContentType = "application/json",
    };
    var map = new AgentConfigMap();
    map.ConfigMap.Add(string.Empty, file);
    return new EffectiveConfig { ConfigMap = map };
}

// The ratio in a remote config of the form {"sampling_ratio": 0.25}.
static double? SamplingRatio(AgentRemoteConfig config)
{
    if (config.Config?.ConfigMap.TryGetValue(string.Empty, out var file) != true)
    {
        return null;
    }

    try
    {
        using var doc = JsonDocument.Parse(file.Body.Memory);
        var ratio = doc.RootElement.GetProperty("sampling_ratio").GetDouble();
        return ratio is >= 0 and <= 1 ? ratio : null;
    }
    catch (Exception e) when (e is JsonException or KeyNotFoundException or InvalidOperationException)
    {
        return null;
    }
}

/// <summary>A sampler the server can change while the program runs.</summary>
internal sealed class LiveSampler(double ratio) : Sampler
{
    public volatile Sampler Current = new TraceIdRatioBasedSampler(ratio);

    public override SamplingResult ShouldSample(in SamplingParameters parameters) => this.Current.ShouldSample(parameters);
}
