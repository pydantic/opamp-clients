# Fleet quickstart: change a running app from Logfire

A tiny shop that takes an order a second, in Python, Node.js and Go. Connect it to Logfire Fleet,
watch its orders in Live view, then change its policy in Fleet and watch the app change without a
restart or a deploy.

Each order is a span, `checkout order N`, with an INFO log inside it (`order N paid`). Every fifth
order is slow and logs a warning too (`order N took longer than 2 s`). The app prints
`order N sent` to its terminal, so you can see it running.

## 1. Connect it in Fleet

In your Logfire project, open **Fleet**, choose **Connect an application**, name it
`fleet-quickstart`, and pick the setup that matches the sample you'll run: **Python • Logfire**,
**Node.js • OpenTelemetry**, or **Go • OpenTelemetry**. Leave the policy as
**New policy**. Fleet then shows:

- the install command for the integration (run it in the sample's `python`, `node` or `go`
  directory),
- the commands that set the OpAMP endpoint and your connection key (`OTEL_OPAMP_ENDPOINT`,
  `OTEL_OPAMP_HEADERS`),
- under **Set up telemetry to Logfire**, the commands that set a write token so the app's
  telemetry reaches Logfire.

Run those in one terminal, then start the app from the same terminal.

## 2. Run the sample

**Python** (Python 3.11 or later, in a virtual environment where Fleet's install command ran):

```bash
cd python
python app.py
```

**Node.js** (Node.js 22 or later; run Fleet's install command in `node`, which adds
`@pydantic/logfire-opamp-otel`):

```bash
cd node
npm install
npm start
```

**Go** (Go 1.26 or later; the module comes from the Go module proxy, so `go run` fetches it):

```bash
cd go
export OTEL_SERVICE_NAME=fleet-quickstart OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
go run .
```

Within a few seconds the app prints a connected line (`OpAMP: connected to ...` in Python and
Node.js, `opamp: connected` in Go) and Fleet's
dialog moves to **Application connected**. Choose **Check telemetry in Live view**: an order
arrives every second, each with its INFO line, and a warning every fifth.

## 3. Change it from Fleet

1. In Fleet, open the application's policy (**Policies**, then the policy you created).
2. Set **Minimum log level** to **Warning** and publish the change.
3. Watch Live view. Within a few seconds the `order N paid` lines stop. The spans and the
   `took longer than 2 s` warnings keep coming. The app kept running the whole time; the
   terminal still prints every order.

The application's page in Fleet shows the new policy as **Applied**. Set the level back to the
SDK's own and publish again, and the INFO lines come back.

More to try on the same policy:

| Change | What you see in Live view |
|---|---|
| **Traces to sample (%)** at 20 | about one order in five |
| Traces **Pause exports** | no spans; logs go on |

[docs/sdk-settings.md](../../docs/sdk-settings.md) lists every control, and
[docs/guides/logfire.md](../../docs/guides/logfire.md#logfire-fleet) covers Fleet in full.

## If it doesn't connect

- No connected line: check `OTEL_OPAMP_ENDPOINT` and the connection key in
  `OTEL_OPAMP_HEADERS`. A wrong key makes the app warn and name `OTEL_OPAMP_HEADERS`.
- Connected, but nothing in Live view: the write token is missing or for another project
  (`LOGFIRE_TOKEN` for Python, `OTEL_EXPORTER_OTLP_HEADERS` for Node.js and Go).
- The policy shows **Failed** in Fleet: its message names the control the app can't apply.
  [docs/troubleshooting.md](../../docs/troubleshooting.md#a-policy-is-failed) has the common ones.
