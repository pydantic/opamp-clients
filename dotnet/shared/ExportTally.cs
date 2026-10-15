// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Pydantic.OpAmp.Client.Sdk;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// Export health's counts, per signal, cumulative since start: what the gates saw of each export
/// attempt (the OTLP exporter's retry is a new request through the gate, so each counts), and the
/// items dropped on purpose (a level, a cap) at the gates and the sampler's trace cap.
/// </summary>
internal sealed class ExportTally
{
    private readonly object gate = new();
    private readonly SignalCounts traces = new();
    private readonly SignalCounts metrics = new();
    private readonly SignalCounts logs = new();

    /// <summary>An export the collector accepted.</summary>
    public void Ok(Signal signal)
    {
        lock (this.gate)
        {
            var c = this.Of(signal);
            c.ExportsOk++;
            c.LastFailed = false;
        }
    }

    /// <summary>An export that failed (<paramref name="error"/>, the exporter's or the collector's), of <paramref name="items"/> items.</summary>
    public void Failed(Signal signal, long items, string error)
    {
        lock (this.gate)
        {
            var c = this.Of(signal);
            c.ExportsFailed++;
            c.ItemsFailed += (ulong)Math.Max(0, items);
            c.LastFailed = true;
            c.LastError = error;
        }
    }

    /// <summary>Items dropped on purpose.</summary>
    public void Filtered(Signal signal, long items)
    {
        if (items <= 0)
        {
            return;
        }

        lock (this.gate)
        {
            this.Of(signal).ItemsFiltered += (ulong)items;
        }
    }

    /// <summary>The counts now, for the layer's Health call.</summary>
    public ExportCounts Counts()
    {
        lock (this.gate)
        {
            return new ExportCounts { Traces = this.traces.Clone(), Metrics = this.metrics.Clone(), Logs = this.logs.Clone() };
        }
    }

    private SignalCounts Of(Signal signal) => signal switch
    {
        Signal.Traces => this.traces,
        Signal.Metrics => this.metrics,
        _ => this.logs,
    };
}
