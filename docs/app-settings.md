# Application settings

An OpAMP server can send an application settings of its own, beside the SDK's: a feature flag, a
log verbosity, a limit. The SDK doesn't read them; it hands them to the application, which reads
one by its key, with a default, and can ask to be told when they change. This page is the
contract every integration keeps: what the server sends, what the layer checks, and the API each
language gives the application.

## On the wire

They are the `custom` setting of the settings document, the remote config's `elastic` entry
([sdk-settings.md](sdk-settings.md#the-document)): a JSON object of keys to values.

```json
{"sampling_rate": "0.25", "custom": {"checkout.new_flow": "on", "example.log_level": "debug"}}
```

A server that sends only strings (Kibana, o11yfleet's renderer) may send the object encoded in a
string, as it does `sampling_rules`:

```json
{"custom": "{\"checkout.new_flow\": \"on\"}"}
```

- **Keys**: ASCII letters, digits, `.`, `_` and `-`, starting with a letter or a digit, 1 to 128
  bytes. Case-sensitive: `Flag` and `flag` are two keys. Dots are a convention for namespaces
  (`checkout.new_flow`), nothing more.
- **Values** are text: a string is kept exactly as sent (not trimmed, an empty string included),
  a number or a boolean as its JSON text (`25` is `"25"`, `true` is `"true"`, `1e21` is
  `"1e+21"`: send strings to keep the text you mean). At most 4096 bytes (UTF-8) each.
- At most **128 entries**. A `null` entry is that key left out (it still counts towards the 128).
- `custom` left out, `null`, `{}` or a blank string is the setting left out: **every key back to
  the application's default**. So is a remote config with no `elastic` entry.

What is wrong is refused as every other wrong setting is
([sdk-settings.md](sdk-settings.md#the-document)): the whole config is answered **FAILED**, naming
the entry, and nothing changes, the application's values included. `custom` is a setting the
layer knows, so a bad value of it is a bad value, not an unknown key; and applying a map with an
entry missing would leave the application reading a mix the server never sent.

| Sent | FAILED with |
|---|---|
| not an object (or one in a string) | `custom: must be a JSON object of keys to values, or one in a string` |
| a string that isn't an object | `custom: not a JSON object of keys to values` |
| a bad key | `custom: "a b" is not a key (ASCII letters, digits, ...)` |
| a key sent twice | `custom.flag: sent more than once` |
| an array or object as a value | `custom.flag: values must be strings, numbers or booleans` |
| a value over 4096 bytes | `custom.flag: 4097 bytes, at most 4096` |
| over 128 entries | `custom: 129 entries, at most 128` |

`testdata/sdk-settings/custom.json` holds each of these, and the maps taken, as shared
vectors; [examples/settings/flip-a-feature-flag.json](../examples/settings/flip-a-feature-flag.json)
is a whole document.

## Through the layer

`custom` is a **live** setting a host has to **declare**, as every setting is ([the
host](sdk-settings.md#the-host)): an integration names `custom` in `Host.live` where the
application can read the values. Undeclared (an integration that doesn't have this API yet), it
is set aside: unchecked, named in `Settings.unsupported`, never reported in force, and the rest of
the config still APPLIED.

Declared, the layer reads and checks it with the rest of the document, and hands the map over in
the typed settings, `Settings.custom` (a `map<string, string>`, field 16 of
`proto/opamp/sdk/v1/settings.proto`), in every reply that carries settings: `Loaded.settings`
(at start, from the saved document), `Decision.apply` (APPLIED) and `Refreshed.settings`. There
is no new call and no new WebAssembly or C export: an integration reads one more field of what
it already gets. Debug mode leaves it alone.

The **effective config** reports it as the other structured settings are reported, a string
holding compact JSON, keys sorted, every value a string, so the server sees exactly what the SDK
took (`25` sent is `"25"` taken):

```json
{"custom": "{\"checkout.new_flow\":\"on\",\"example.log_level\":\"debug\"}", "sampling_rate": "0.25"}
```

The same map sent another way (in a string, keys in another order, `25` for `"25"`) is the same
text, so it is no change.

**Restarts.** The integrations already save the document in force for the next start (in the
state directory, `sdk-settings.json` in Python) and `Load` it before the application's code runs.
`custom` is in it, so with a state directory the values in force at exit are in force again from
the start, before any config arrives and whether the server can be reached or not. Without one,
each start begins from the defaults until the server sends a config (it is live, so it needs no
state to be applied).

## The API

Every integration gives the application the same five things, named as its language names them:

- **`get(key, default)`**: the value in force, or `default` while there is none: before the first
  config (and no saved one), when the config in force leaves the key out, without an OpAMP server
  configured at all, and in an integration that isn't managed. Always a string, never an error,
  whatever the key.
- **Typed reads** (optional, but where given, exactly these rules, so that the same text reads
  the same in every language; `custom.json`'s `typed` vectors pin them, and the layer's own
  typed reads are the reference). Spaces round the value
  (space, tab, CR, LF; no other) are ignored. Anything else is `default`; none of them errs:
  - bool: `true` or `false` in any ASCII case (not `on`, `1` or `yes`);
  - int: decimal digits with an optional sign, in the 64-bit signed range (leading zeros
    allowed; no `_`, no hex);
  - float: a JSON number with an optional leading `+`, finite as a 64-bit float (`1e400` is
    the default).
- **A snapshot**: every key in force, as one map no later change alters.
- **`on_change(callback)`**: the callback gets the map now in force (read-only) and the keys
  added, changed or removed. It returns a way to unsubscribe.
  - Called after a change is put in force, once per change, whatever the cause: a config
    APPLIED, the saved settings loaded at start (if the callback was added before that, which in
    most integrations it can't be), a forked worker following its parent. Not called when a
    config leaves the map as it was (the same map again, another setting changed, a FAILED
    config), and not called at subscription with the values already in force: read the snapshot
    for those.
  - Changes are told one at a time, in the order they happened, each callback in the order it was
    added; `get` already returns the new values inside the callback.
  - The callback runs on the integration's thread (its OpAMP client's, in most), so it should be
    quick: hand longer work to the application's own threads.
  - A callback that throws (raises, panics) is caught and logged as an error; the other callbacks
    are still called, the config is still APPLIED and the client goes on. A callback may
    subscribe or unsubscribe callbacks, itself included, while it runs; that takes effect from the
    next change.
- **Thread safety**: any thread may read, subscribe and unsubscribe at any time. A reader sees
  the whole of one map or the whole of the next, never part of each.

| Language | Package | Read | Typed | Snapshot | Told of changes |
|---|---|---|---|---|---|
| Python | `pydantic-opamp-otel` | `opamp_otel.app_settings.get(key, default=None)` | `get_bool`, `get_int`, `get_float` | `app_settings.snapshot()` → `Mapping[str, str]` | `app_settings.on_change(cb)` → a function that unsubscribes; `cb(settings, changed: frozenset[str])` |
| Go | `github.com/pydantic/opamp-clients/go/otel` | `opampotel.AppSetting(key, def string) string` | `AppSettingBool`, `AppSettingInt` (`int64`), `AppSettingFloat` (`float64`) | `opampotel.AppSettings() map[string]string` (a copy) | `opampotel.OnAppSettingsChange(func(settings map[string]string, changed []string)) (remove func())` |
| Node.js | `@pydantic/opamp-otel` | `appSettings.get(key, def)` | `getBool`, `getInt`, `getFloat` | `appSettings.snapshot()` → a frozen `Readonly<Record<string, string>>` | `appSettings.onChange((settings, changed: string[]) => void)` → `() => void` |
| Java | the Java agent extension; the application compiles against its `app-settings` jar | `dev.pydantic.opamp.AppSettings.get(String key, String def)` | `getBoolean`, `getLong`, `getDouble` | `AppSettings.snapshot()` → an unmodifiable `Map<String, String>` | `AppSettings.onChange(AppSettingsListener)` → `AutoCloseable`; `void changed(Map<String, String> settings, Set<String> changed)`, the keys sorted |
| .NET | `Pydantic.OpAmp.AutoInstrumentation`, `WithOpAmp` (the type is in `Pydantic.OpAmp.Client`, which both bring) | `Pydantic.OpAmp.OpAmpAppSettings.Get(string key, string? defaultValue)` | `GetBool`, `GetInt64`, `GetDouble` | `OpAmpAppSettings.Snapshot()` → `IReadOnlyDictionary<string, string>` | `OpAmpAppSettings.OnChange(Action<IReadOnlyDictionary<string, string>, IReadOnlySet<string>>)` → `IDisposable`; the keys enumerated in ordinal order |

All five are built (Node.js: [node-otel.md](node-otel.md#the-applications-own-settings), Java:
[javaagent.md](javaagent.md#the-applications-own-settings), .NET:
[dotnet-otel.md](dotnet-otel.md#the-applications-own-settings)). Where a language's integration lives in
another class loader or assembly than the application's code (the Java agent, .NET's automatic
instrumentation), the API is a small type the application can reference and the integration
fills; with no integration attached, it answers every read with its default and never calls a
callback. In Java the extension puts one class of the JDK's types alone on the bootstrap class
path, which every class loader reaches, and fills that; the application's `AppSettings` (one per
class loader that has it) finds it there. Node.js's `getInt` takes only safe integers (±2^53 - 1): beyond them it is the default.

In Python:

```python
from opamp_otel import app_settings

if app_settings.get("checkout.new_flow", "off") == "on":
    ...

def changed(settings, keys):
    if "example.log_level" in keys:
        logging.getLogger("app").setLevel(settings.get("example.log_level", "INFO").upper())

unsubscribe = app_settings.on_change(changed)
```

It works whoever built the SDK (`opentelemetry-instrument`, `opamp_otel.configure()`, another
configurator, `OTEL_CONFIG_FILE`): the package declares `custom` always. A forked worker
(gunicorn's) follows its parent's values, and its own callbacks are called there. Under
`opentelemetry-instrument` the saved values are loaded before the application's code runs, so a
callback the application adds is told of the server's changes from then on, not of those.

## How it is checked

- The settings layer is held to every refusal and limit above, and to the shared vectors
  (`testdata/sdk-settings/custom.json`), before it is released.
- Each language: its wrapper runs `custom.json` (Python `python/tests/test_sdk_settings.py`,
  Node.js `node/opamp/test/sdk.test.ts`), and its integration's tests cover the API (Python
  `python/otel/tests/test_app_settings.py`: reads before and after a config, the typed vectors,
  once per change and never for the same map, callbacks that raise, order, unsubscribing,
  threads, restarts, forked workers; Go `go/otel/appsettings_test.go`, the same but forked
  workers, with every `custom.json` case through the Go wrapper's `Decide`, a Refresh, `Setup`,
  and `-race`; Node.js `node/otel/test/app-settings.test.ts`: the same, rejected promises in place
  of threads and forks; Java `java/opamp-javaagent/src/test`, `AppSettingsTest` and
  `AppSettingsExtensionTest`: the same but forked workers, with every `custom.json` case through
  the extension's `Decide`, a refresh, a failed setup, several class loaders, and the bridge
  loaded where only the JDK is seen; .NET
  `dotnet/Pydantic.OpAmp.AutoInstrumentation.Tests/AppSettingsTests.cs`, the same but forked
  workers, with every `custom.json` case through the .NET wrapper's `Decide` and the plugin's
  agent).
- End to end, `check_app_settings` in [examples/integrations.py](../examples/integrations.py):
  the example application reads the flag `example.feature` and, while it is `on`, requests
  `/feature` each round; it requests `/changed` once per change it is told of. The server turns
  the flag on, sends a bad map (FAILED, the flag stays on), leaves `custom` out (the flag back to
  `off`), sends it in a string (on again) and the same map again (nothing to tell): three
  `/changed`. After a restart with no offer the flag is on from the start. An integration opts in
  by giving its example application that behaviour and calling `check_app_settings(launch,
  target)` from its `try.py`, as every integration's `examples/*/try.py` does (its check `app`).
