# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT

import asyncio
import json
import os
import signal
import sys
import uuid

from pydantic_opamp_client import Client, Config, Notification, Status, anyvalue_pb2, opamp_pb2
from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SpanExporter, SpanExportResult
from opentelemetry.sdk.trace.sampling import ParentBased, Sampler, TraceIdRatioBased

IDENTIFYING = {"service.name", "service.namespace", "service.instance.id"}


class LiveSampler(Sampler):
    """A sampler the server can change while the program runs."""

    def __init__(self, ratio: float) -> None:
        self.current: Sampler = TraceIdRatioBased(ratio)

    def should_sample(self, *args, **kwargs):
        return self.current.should_sample(*args, **kwargs)

    def get_description(self) -> str:
        return self.current.get_description()


class LiveExporter(SpanExporter):
    """An exporter the server can point elsewhere while the program runs. Until it says where,
    spans go nowhere."""

    def __init__(self) -> None:
        self.current: SpanExporter | None = None

    def set(self, to: SpanExporter) -> None:
        """Point it at `to`, and shut down the exporter it replaces."""
        old, self.current = self.current, to
        if old is not None:
            old.shutdown()

    def export(self, spans):
        current = self.current
        return current.export(spans) if current is not None else SpanExportResult.SUCCESS

    def shutdown(self) -> None:
        if self.current is not None:
            self.current.shutdown()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return self.current.force_flush(timeout_millis) if self.current is not None else True


def exporter(settings: opamp_pb2.TelemetryConnectionSettings) -> SpanExporter:
    """An OTLP/HTTP exporter for the traces part of telemetry settings: the endpoint is the full
    URL (.../v1/traces), with the headers offered."""
    headers = {h.key: h.value for h in settings.headers.headers}
    return OTLPSpanExporter(endpoint=settings.destination_endpoint, headers=headers)


def description(resource: Resource) -> opamp_pb2.AgentDescription:
    """The agent description, from the SDK's resource."""
    d = opamp_pb2.AgentDescription()
    for key, value in resource.attributes.items():
        kv = anyvalue_pb2.KeyValue(key=key, value=anyvalue_pb2.AnyValue(string_value=str(value)))
        (d.identifying_attributes if key in IDENTIFYING else d.non_identifying_attributes).append(kv)
    return d


def effective(settings: dict) -> opamp_pb2.EffectiveConfig:
    body = json.dumps(settings).encode()
    return opamp_pb2.EffectiveConfig(config_map=opamp_pb2.AgentConfigMap(
        config_map={"": opamp_pb2.AgentConfigObject(body=body, content_type="application/json")}))


async def main() -> None:
    # One UUID is both the OpAMP instance UID and service.instance.id, so the fleet view and the
    # telemetry name the same instance.
    uid = uuid.uuid4()
    resource = Resource.create({"service.name": "checkout", "service.instance.id": str(uid)})
    sampler = LiveSampler(1.0)
    provider = TracerProvider(resource=resource, sampler=ParentBased(root=sampler))
    # Spans go wherever the server says (telemetry settings), through a live exporter.
    live = LiveExporter()
    provider.add_span_processor(BatchSpanProcessor(live, schedule_delay_millis=500))
    trace.set_tracer_provider(provider)

    client: Client

    def on_note(n: Notification) -> None:  # called on the event loop: keep it quick
        if n.kind == "stopped" and n.error is not None:
            # The client stopped on its own (a bug): don't carry on unmanaged.
            print("OpAMP client stopped:", n.error, file=sys.stderr)
            os._exit(1)
        if n.kind == "connection_settings":
            # Where to send our own telemetry: all the settings, not only what changed.
            settings = n.connection_settings
            answer = opamp_pb2.ConnectionSettingsStatus(last_connection_settings_hash=settings.hash,
                                                        status=opamp_pb2.ConnectionSettingsStatuses_APPLIED)
            if settings.HasField("own_traces"):
                try:
                    live.set(exporter(settings.own_traces))
                except Exception as e:  # noqa: BLE001 - any reason is the answer
                    answer.status = opamp_pb2.ConnectionSettingsStatuses_FAILED
                    answer.error_message = str(e)
            client.set_connection_settings_status(answer)
            return
        if n.kind != "remote_config":
            return
        status = opamp_pb2.RemoteConfigStatus(last_remote_config_hash=n.remote_config.config_hash)
        try:
            settings = json.loads(n.remote_config.config.config_map[""].body)
            ratio = float(settings["sampling_ratio"])
            if not 0 <= ratio <= 1:
                raise ValueError(ratio)
        except (KeyError, TypeError, ValueError):
            status.status = opamp_pb2.RemoteConfigStatuses_FAILED
            status.error_message = 'want {"sampling_ratio": 0..1}'
        else:
            sampler.current = TraceIdRatioBased(ratio)
            status.status = opamp_pb2.RemoteConfigStatuses_APPLIED
            client.set_effective_config(effective({"sampling_ratio": ratio}))
        client.set_remote_config_status(status)

    client = Client.start(
        Config(
            endpoint=os.environ["OPAMP_ENDPOINT"],  # https://.../v1/opamp, or wss://... for WebSocket
            instance_uid=uid.bytes,
            capabilities=opamp_pb2.AgentCapabilities_AcceptsRemoteConfig | opamp_pb2.AgentCapabilities_ReportsOwnTraces,
            # headers={"Authorization": f"Bearer {token}"},
        ),
        Status(
            description=description(resource),
            health=opamp_pb2.ComponentHealth(healthy=True),
            effective_config=effective({"sampling_ratio": 1.0}),
        ),
        on_note,
    )

    # The application's work: a span every 100 ms.
    async def work() -> None:
        tracer = trace.get_tracer("checkout")
        while True:
            tracer.start_span("checkout").end()
            await asyncio.sleep(0.1)

    worker = asyncio.create_task(work())

    # The program runs until interrupted.
    stop = asyncio.Event()
    asyncio.get_running_loop().add_signal_handler(signal.SIGINT, stop.set)
    await stop.wait()
    worker.cancel()

    # Stop: the server hears the agent go (agent_disconnect), then the SDK flushes.
    await client.stop()
    provider.shutdown()


asyncio.run(main())
