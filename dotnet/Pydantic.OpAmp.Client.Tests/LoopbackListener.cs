// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Net;
using System.Net.Sockets;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// An <see cref="HttpListener"/> on a free loopback port. A port found free can be taken before
/// the listener binds it (by a test running beside this one, or by a client's outgoing
/// connection): then another port is tried, on a new listener. Done with one, Close it, never
/// Stop it: on Linux a Stop and then the Dispose each remove the prefix, and the second binds the
/// port again, throwing "Address already in use" if something has taken it meanwhile.
/// </summary>
internal static class LoopbackListener
{
    public static (HttpListener Listener, int Port) Start()
    {
        for (var attempt = 1; ; attempt++)
        {
            var port = FreePort();
            var listener = new HttpListener();
            listener.Prefixes.Add($"http://127.0.0.1:{port}/");
            try
            {
                listener.Start();
                return (listener, port);
            }
            catch (HttpListenerException) when (attempt < 20)
            {
                try
                {
                    ((IDisposable)listener).Dispose();
                }
                catch (HttpListenerException)
                {
                    // it never listened
                }
            }
        }
    }

    /// <summary>
    /// Closes a listener already stopped. On Linux, closing after <see cref="HttpListener.Stop"/>
    /// looks the port up again and binds it anew; when something else holds it by then (a test
    /// running beside this one, or a client's outgoing connection) that throws, though the stop
    /// left nothing to release.
    /// </summary>
    public static void Close(HttpListener listener)
    {
        try
        {
            listener.Close();
        }
        catch (HttpListenerException)
        {
            // the port was taken after the stop released it
        }
    }

    private static int FreePort()
    {
        var l = new TcpListener(IPAddress.Loopback, 0);
        l.Start();
        var port = ((IPEndPoint)l.LocalEndpoint).Port;
        l.Stop();
        return port;
    }
}
