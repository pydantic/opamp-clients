// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Runtime.InteropServices;
using Pydantic.OpAmp.Client;
using Pydantic.OpAmp.Client.Sdk;

// The SDK settings layer and a client both load the native core from the package's runtimes/<rid>.
using var settings = new SdkSettings();
var description = settings.Describe([new OpAmp.Proto.V1.KeyValue { Key = "service.name", Value = new OpAmp.Proto.V1.AnyValue { StringValue = "smoke" } }]);
if (description.IdentifyingAttributes.Count == 0)
{
    throw new InvalidOperationException($"no identifying attributes: {description}");
}

// A client against a port nothing listens on: it starts (the core ran) and stops.
var client = OpAmpClient.Start(new OpAmpClientOptions { Endpoint = new Uri("http://127.0.0.1:9/v1/opamp") }, new OpAmpStatus());
await client.StopAsync();
Console.WriteLine($"ok: {RuntimeInformation.RuntimeIdentifier} {RuntimeInformation.OSDescription}");
