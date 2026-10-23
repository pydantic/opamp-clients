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
| `signal_exports.traces`, `.metrics`, `.logs` | A signal switched off goes no further down the pipeline. |
| `log_levels` | By the scope a record comes under (`ScopeLogs`' scope name) and its SeverityNumber, the exact scope name first, then the longest prefix; a record with no severity is kept, but under `off`. |
| `attribute_redaction` | On each span's attributes, its events' and its links', and each log record's: a dropped key goes, a masked one's value becomes the string `[REDACTED]`. Not the resource's or the scope's attributes, and never metrics' (masking would merge series), as in an SDK. |

Not carried out, and refused:

- `head_sampling` and `sampling_rules`. In an SDK the ratio and the rules decide only a new trace,
  and a span whose parent was sampled (by a rule, a sampled caller or debug mode) is kept at any
  ratio. A collector sees only finished spans, so it can't tell those apart from a trace the ratio
  would drop: sampling again by trace ID would drop traces the same policy keeps in an app. Nor do
  the SDKs' own trace-ID ratio samplers agree on the trace ID's bits: Go, Rust and Python read its
  low 64 bits, Java the absolute value of the low 64 bits as a signed number, .NET the high 64
  bits, Node.js an XOR of its four 32-bit words. So a collector's ratio would not keep the traces
  an SDK's ratio kept. Sample in the SDKs.
- `trace_rate_limit` and `log_rate_limit`: a collector's cap would be per collector, and on top
  of an SDK's.
- `debug`, `app_settings` and `instrumentations`: nothing in a collector to carry them out.
- `scrubbing`, for now.

What a collector carries out differently from an SDK, because it sees more than one
application's telemetry:

- One policy covers everything going through the collector, every service's alike. For
  telemetry an SDK already filtered under the same policy, filtering again changes nothing.
- `log_levels` goes by the scope name and SeverityNumber. Records from a collector's own
  receivers (files, syslog, Kubernetes events) often have neither, so they are kept unless
  their scope is `off`.
- `attribute_redaction` matches top-level keys only: not keys inside a map value, and not a log
  record's body. Attributes a later processor adds (the processor goes first) are not redacted.

## Config

```yaml
processors:
  logfirepolicy:
    policy: '{"schema":"logfire-sdk-policy/v1","head_sampling":{"mode":"local"},"signal_exports":{"traces":{"mode":"local"},"metrics":{"mode":"local"},"logs":{"mode":"local"}},"attribute_redaction":{"mode":"override","mask":["*token*"]}}'
```

`policy` is the document as a server sends it. A config whose policy the layer refuses doesn't
validate. The processor keeps nothing: a new policy is a new collector config.

Build it into a collector with ocb, beside the Supervisor's [extension](../opampextension/):

```yaml
processors:
  - gomod: github.com/pydantic/opamp-clients/go/logfirepolicyprocessor v0.1.0
```

## Tests

```bash
scripts/fetch_core.py fetch
(cd go/logfirepolicyprocessor && go test -race ./...)
```
