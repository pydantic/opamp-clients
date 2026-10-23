# logfirepolicy processor

`github.com/pydantic/opamp-clients/go/logfirepolicyprocessor`: a Collector processor that carries
out a Logfire SDK policy (`logfire-sdk-policy/v1`, [sdk-settings.md](../../docs/sdk-settings.md))
on the telemetry going through it, as an SDK under the same policy does. The policy is read and
checked by the SDK settings layer, the one every SDK integration runs, and matched with the
matchers the Go integration uses (`go/opamp/sdksettings/match`); the shared vectors in
`testdata/sdk-settings` hold the processor to them. Package `sdkpolicy` holds the policy reading alone (what the
processor carries out, and the agent description it implies), for the Supervisor, which links it
without the Collector's processor packages.

Under the [Supervisor](../opampsupervisor/README.md) nothing needs configuring: when the server
sends the policy in the `logfire-sdk-policy.json` entry of the config map, beside the collector's
YAML or alone, the Supervisor configures this processor with it and puts it first in every
traces, metrics and logs pipeline. It tells the server the collector takes the policy only when
the collector lists the processor among its available components.

## The controls

A collector carries out the controls that mean the same in a collector as in an SDK. A policy
that overrides any other is refused, with the control's name first, as an SDK that can't carry a
control out refuses it (`sampling_rules: rules: this SDK cannot apply the requested override`), and
the agent description's `logfire.opamp.supported_controls` lists only these, so a server doesn't
send one.

| Control | In the processor |
|---|---|
| `head_sampling` | Keeps a trace by its trace ID alone, by the OpenTelemetry SDK's trace-ID ratio sampler at the ratio: the trace ID's low 64 bits, as the SDKs' ratio samplers read them. So an SDK and a collector under the same ratio keep the same traces (0.5 then 0.5 keeps half, not a quarter), and every span of a trace is kept or dropped together, on any collector. |
| `signal_exports.traces`, `.metrics`, `.logs` | A signal switched off goes no further down the pipeline. |
| `log_levels` | By the scope a record comes under (`ScopeLogs`' scope name) and its SeverityNumber, the exact scope name first, then the longest prefix; a record with no severity is kept, but under `off`. |
| `attribute_redaction` | On each span's attributes, its events' and its links', and each log record's: a dropped key goes, a masked one's value becomes the string `[REDACTED]`. Not the resource's or the scope's attributes, and never metrics' (masking would merge series), as in an SDK. |

Not carried out, and refused: `sampling_rules` (decided on the root span as it starts; a
collector may see a trace's other spans first, or not see the root at all), `trace_rate_limit`
and `log_rate_limit` (a collector's cap would be per collector, and on top of an SDK's), `debug`,
`app_settings` and `instrumentations` (nothing in a collector to carry them out).

## Config

```yaml
processors:
  logfirepolicy:
    policy: '{"schema":"logfire-sdk-policy/v1","head_sampling":{"mode":"override","ratio":0.25},"signal_exports":{"traces":{"mode":"local"},"metrics":{"mode":"local"},"logs":{"mode":"local"}},"attribute_redaction":{"mode":"override","mask":["*token*"]}}'
```

`policy` is the document as a server sends it. A config whose policy the layer refuses doesn't
validate. The processor keeps nothing: a new policy is a new collector config.

Build it into a collector with ocb, beside the Supervisor's [extension](../opampextension/):

```yaml
processors:
  - gomod: github.com/pydantic/opamp-clients/go/logfirepolicyprocessor v0.2.0
```

## Tests

```bash
scripts/fetch_core.py fetch
(cd go/logfirepolicyprocessor && go test -race ./...)
```
