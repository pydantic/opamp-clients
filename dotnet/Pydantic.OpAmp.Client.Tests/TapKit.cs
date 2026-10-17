// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using System.Text.Json;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Core;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>One record of a core tap: its kind, and its bytes.</summary>
internal sealed record TapRecord(int At, byte Kind, byte[] Bytes)
{
    /// <summary>A file record's operation (kind 5).</summary>
    public JsonElement Op => JsonDocument.Parse(this.Bytes).RootElement;

    public string Field(string name) => this.Op.TryGetProperty(name, out var v) ? v.ToString() : string.Empty;

    public override string ToString() => this.Kind == FileJournal.Kind
        ? System.Text.Encoding.UTF8.GetString(this.Bytes)
        : this.Kind switch
        {
            3 => $"step in: {Step.Parser.ParseFrom(this.Bytes).Input.KindCase}",
            _ => $"kind {this.Kind}",
        };
}

/// <summary>Reading a core tap (its records, the saves and answers in it) and checking it with opamp-tap.</summary>
internal static class TapKit
{
    /// <summary>The tap file a client given <paramref name="path"/> wrote (the process's first, or a later one's).</summary>
    public static string FileOf(string path)
    {
        var dir = Path.GetDirectoryName(path)!;
        var name = Path.GetFileName(path);
        return Directory.GetFiles(dir, name + "*").Single();
    }

    public static List<TapRecord> Read(string path)
    {
        var file = File.ReadAllBytes(FileOf(path));
        var records = new List<TapRecord>();
        for (var at = 0; at < file.Length;)
        {
            var len = (int)System.Buffers.Binary.BinaryPrimitives.ReadUInt32LittleEndian(file.AsSpan(at + 1));
            records.Add(new TapRecord(records.Count, file[at], file.AsSpan(at + 13, len).ToArray()));
            at += 13 + len;
        }

        return records;
    }

    /// <summary>The file records, with the op <paramref name="op"/> if given.</summary>
    public static List<TapRecord> Files(IEnumerable<TapRecord> records, string? op = null) =>
        records.Where(r => r.Kind == FileJournal.Kind && (op is null || r.Field("op") == op)).ToList();

    /// <summary>The steps' inputs, with the record each is.</summary>
    public static List<(TapRecord Record, Input Input)> Inputs(IEnumerable<TapRecord> records) =>
        records.Where(r => r.Kind == 3).Select(r => (r, Step.Parser.ParseFrom(r.Bytes).Input)).ToList();

    /// <summary>The effects the core asked for, in order, with the record that asked.</summary>
    public static List<(TapRecord Record, Effect Effect)> Effects(IEnumerable<TapRecord> records) =>
        records.Where(r => r.Kind is 2 or 4)
            .SelectMany(r => (Reply.Parser.ParseFrom(r.Bytes).Output ?? new Output()).Effects.Select(e => (r, e)))
            .ToList();

    /// <summary>The core's saves: id and data, in order.</summary>
    public static List<(ulong Id, byte[] Data)> Stores(IEnumerable<TapRecord> records) =>
        Effects(records).Where(e => e.Effect.KindCase == Effect.KindOneofCase.Store)
            .Select(e => (e.Effect.Store.Id, e.Effect.Store.Data.ToByteArray())).ToList();

    /// <summary>
    /// The opamp-tap checker: OPAMP_TAP_BIN, or target/release/opamp-tap above the tests; null if
    /// there is none that runs here.
    /// </summary>
    public static string? Checker()
    {
        if (Environment.GetEnvironmentVariable("OPAMP_TAP_BIN") is { Length: > 0 } bin)
        {
            return bin;
        }

        for (var dir = new DirectoryInfo(AppContext.BaseDirectory); dir is not null; dir = dir.Parent)
        {
            var candidate = Path.Combine(dir.FullName, "target", "release", "opamp-tap");
            if (File.Exists(candidate))
            {
                return candidate;
            }
        }

        return null;
    }

    /// <summary>Run opamp-tap on the tap at <paramref name="path"/>: its exit code and output, or null if there is no checker.</summary>
    public static (int Code, string Output)? Check(string path)
    {
        if (Checker() is not { } bin)
        {
            return null;
        }

        var start = new ProcessStartInfo(bin, [FileOf(path)]) { RedirectStandardOutput = true, RedirectStandardError = true };
        Process p;
        try
        {
            p = Process.Start(start)!;
        }
        catch (System.ComponentModel.Win32Exception)
        {
            return null; // built for another system
        }

        using (p)
        {
            var stderr = p.StandardError.ReadToEndAsync();
            var stdout = p.StandardOutput.ReadToEnd();
            p.WaitForExit();
            var output = stdout + stderr.Result;

            // A binary for another system that a container's binfmt handler starts and refuses
            // (a macOS build, under OrbStack): no checker that runs here.
            return p.ExitCode != 0 && output.Contains("Exec format error", StringComparison.Ordinal) ? null : (p.ExitCode, output);
        }
    }

    /// <summary>A package offer: package "p" at <paramref name="url"/>, holding <paramref name="file"/>.</summary>
    public static ServerToAgent Offer(AgentToServer msg, string url, byte[] file, string version = "1")
    {
        var reply = new ServerToAgent { InstanceUid = msg.InstanceUid };
        var available = new PackagesAvailable { AllPackagesHash = ByteString.CopyFromUtf8("all-" + version) };
        available.Packages.Add("p", new PackageAvailable
        {
            Type = PackageType.TopLevel,
            Version = version,
            Hash = ByteString.CopyFromUtf8("p-" + version),
            File = new DownloadableFile
            {
                DownloadUrl = url,
                ContentHash = ByteString.CopyFrom(System.Security.Cryptography.SHA256.HashData(file)),
            },
        });
        reply.PackagesAvailable = available;
        return reply;
    }
}
