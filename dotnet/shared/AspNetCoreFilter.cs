// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Linq.Expressions;
using OpenTelemetry.Trace;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// Sampling rules for ASP.NET Core's requests. On .NET 8 the server activity reaches the sampler
/// named <c>Microsoft.AspNetCore.Hosting.HttpRequestIn</c>, with no tags: no path for a rule to
/// match. The ASP.NET Core instrumentation's <c>Filter</c>, though, is given the request's
/// <c>HttpContext</c> as the activity starts. So for a new trace's request activity the sampler
/// records it (<see cref="LiveSampler.FilterDecides"/>), and this Filter decides it with the request's
/// attributes at start (<c>url.path</c>, <c>http.request.method</c>, <c>url.scheme</c>,
/// <c>server.address</c>, <c>server.port</c>): the rules, the rate, the application's sampler, the
/// trace cap. Returning false clears the activity's Recorded flag, so nothing of the trace is
/// exported and its children and outgoing calls follow it as not sampled. The application's own
/// Filter, if it set one, runs first, and its false wins.
/// The options and <c>HttpContext</c> are reached by reflection: neither the plugin's nor the
/// hosting package's callers have to be ASP.NET Core applications.
/// </summary>
internal static class AspNetCoreFilter
{
    /// <summary>The instrumentation's options type, by name.</summary>
    public const string OptionsTypeName =
        "OpenTelemetry.Instrumentation.AspNetCore.AspNetCoreTraceInstrumentationOptions, OpenTelemetry.Instrumentation.AspNetCore";

    // A request's attributes at start, read from an HttpContext: one compiled reader per type.
    private static readonly ConcurrentDictionary<Type, Func<object, KeyValuePair<string, object?>[]>?> Readers = new();

    /// <summary>The options type, if the application has the ASP.NET Core instrumentation; else null.</summary>
    public static Type? OptionsType()
    {
        try
        {
            return Type.GetType(OptionsTypeName, throwOnError: false);
        }
        catch (Exception e) when (e is IOException or BadImageFormatException or TypeLoadException)
        {
            return null;
        }
    }

    /// <summary>
    /// Chain our Filter after the one <paramref name="options"/> has, and let the sampler leave new
    /// requests to it. False, and nothing changed, when the options aren't of the shape known.
    /// </summary>
    public static bool Install(object options, LiveSampler sampler)
    {
        var property = options.GetType().GetProperty("Filter");
        if (property is not { CanRead: true, CanWrite: true } || property.PropertyType is not { IsGenericType: true } type ||
            type.GetGenericTypeDefinition() != typeof(Func<,>) || type.GetGenericArguments()[1] != typeof(bool))
        {
            return false;
        }

        var context = type.GetGenericArguments()[0];
        if (Readers.GetOrAdd(context, Reader) is not { } read)
        {
            return false;
        }

        Func<object, bool>? previous = property.GetValue(options) is Delegate d ? Untyped(d, context) : null;
        Func<object, bool> filter = http => (previous is null || previous(http)) && Decide(http, read, sampler);
        property.SetValue(options, Typed(filter, context, type));
        sampler.FilterDecides = true;
        return true;
    }

    /// <summary>Whether to record the request: true for any activity but a new trace's request, which the sampler left to here.</summary>
    private static bool Decide(object http, Func<object, KeyValuePair<string, object?>[]> read, LiveSampler sampler)
    {
        var activity = Activity.Current;
        if (activity is null || activity.OperationName != LiveSampler.AspNetCoreRequest || activity.ParentSpanId != default)
        {
            return true; // a request with a parent: the sampler followed it already
        }

        KeyValuePair<string, object?>[] tags;
        try
        {
            tags = read(http);
        }
        catch (Exception)
        {
            tags = []; // the rules see no attributes, as the sampler would have
        }

        switch (sampler.DecideRequest(activity, tags))
        {
            case SamplingDecision.Drop:
                return false;
            case SamplingDecision.RecordOnly:
                // Recorded for the processors, as the sampler's RecordOnly would have left it, but
                // not sampled: not exported, and its children and outgoing calls follow that.
                activity.ActivityTraceFlags &= ~ActivityTraceFlags.Recorded;
                return true;
            default:
                return true;
        }
    }

    /// <summary>
    /// <c>ctx => [url.path, http.request.method, url.scheme, server.address, server.port]</c>, as the
    /// instrumentation sets them: the path is <c>(PathBase + Path).ToString()</c>, or <c>/</c>.
    /// Null if <paramref name="context"/> isn't shaped like <c>HttpContext</c>.
    /// </summary>
    private static Func<object, KeyValuePair<string, object?>[]>? Reader(Type context)
    {
        try
        {
            var p = Expression.Parameter(typeof(object), "http");
            var request = Expression.Property(Expression.Convert(p, context), "Request");
            var pathBase = Expression.Property(request, "PathBase");
            var path = Expression.Property(request, "Path");
            var either = Expression.OrElse(Expression.Property(pathBase, "HasValue"), Expression.Property(path, "HasValue"));
            var text = Expression.Condition(
                either,
                Expression.Call(Expression.Add(pathBase, path), pathBase.Type.GetMethod("ToString", Type.EmptyTypes)!),
                Expression.Constant("/"));
            var host = Expression.Property(request, "Host");
            var make = typeof(AspNetCoreFilter).GetMethod(nameof(Tags), System.Reflection.BindingFlags.NonPublic | System.Reflection.BindingFlags.Static)!;
            var body = Expression.Call(
                make,
                text,
                Expression.Property(request, "Method"),
                Expression.Property(request, "Scheme"),
                Expression.Property(host, "Host"),
                Expression.Convert(Expression.Property(host, "Port"), typeof(int?)));
            return Expression.Lambda<Func<object, KeyValuePair<string, object?>[]>>(body, p).Compile();
        }
        catch (Exception e) when (e is ArgumentException or InvalidOperationException)
        {
            return null;
        }
    }

    private static KeyValuePair<string, object?>[] Tags(string path, string method, string scheme, string host, int? port) =>
        port is { } n
            ? [new("url.path", path), new("http.request.method", method), new("url.scheme", scheme), new("server.address", host), new("server.port", n)]
            : [new("url.path", path), new("http.request.method", method), new("url.scheme", scheme), new("server.address", host)];

    /// <summary><c>o => previous((T)o)</c>.</summary>
    private static Func<object, bool> Untyped(Delegate previous, Type context)
    {
        var o = Expression.Parameter(typeof(object), "o");
        return Expression.Lambda<Func<object, bool>>(Expression.Invoke(Expression.Constant(previous), Expression.Convert(o, context)), o).Compile();
    }

    /// <summary><c>(T c) => filter(c)</c>, as the options' delegate type.</summary>
    private static Delegate Typed(Func<object, bool> filter, Type context, Type delegateType)
    {
        var c = Expression.Parameter(context, "c");
        return Expression.Lambda(delegateType, Expression.Invoke(Expression.Constant(filter), Expression.Convert(c, typeof(object))), c).Compile();
    }
}
