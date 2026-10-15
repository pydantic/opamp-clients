// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Diagnostics;
using System.Globalization;
using System.Net;
using System.Net.Security;
using System.Net.Sockets;
using System.Security.Authentication;
using System.Security.Cryptography.X509Certificates;
using System.Text;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The adversary's raw server: what the
/// streams of a Rust mode run connect to. A stream is bytes, not HTTP, so the adversary tells the
/// server what to do in the first line it writes:
/// <code>opamp-raw tag=7 send=1000 chunk=100 gap=5 end=fin</code>
/// The server answers "ok\n", then does what the line says, and keeps for each connection that
/// said who it was (tag) how many bytes it read after the line, whether each was the pattern's
/// (byte i is i % 251), and how the connection ended; the adversary asks for that on another
/// stream (ask=TAG). One server per run, on a port of its own, so every connection is that run's,
/// those that never say who they are too. As go/opamp/adversary_raw_test.go, the reference.
/// </summary>
public partial class AdversaryPropertyTests
{
    /// <summary>
    /// The name the raw server's certificate is for (<see cref="Pki.Server"/>: 127.0.0.1 and
    /// localhost, under <see cref="Pki.Ca"/>, which the client is given to trust), which the
    /// adversary starts TLS under (tls=NAME).
    /// </summary>
    private const string TlsName = "localhost";

    /// <summary>The raw server does what its first line says, and tells what it saw of a connection.</summary>
    // covers: RULE-SHELL-07
    [Fact]
    public async Task TheRawServerDoesWhatItsFirstLineSays()
    {
        using var s = new AdversaryRaw(Pki.Server);
        var mine = new List<TcpClient>();
        try
        {
            async Task<string> LineAsync(TcpClient c)
            {
                using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(20));
                var line = new List<byte>();
                var one = new byte[1];
                while (await c.GetStream().ReadAsync(one, cts.Token) > 0)
                {
                    line.Add(one[0]);
                    if (one[0] == '\n')
                    {
                        break;
                    }
                }

                return Encoding.ASCII.GetString(line.ToArray());
            }

            async Task<TcpClient> DialAsync(string line)
            {
                var c = new TcpClient { NoDelay = true };
                mine.Add(c);
                await c.ConnectAsync(IPAddress.Loopback, s.Port);
                await c.GetStream().WriteAsync(Encoding.ASCII.GetBytes(line + "\n"));
                Assert.Equal("ok\n", await LineAsync(c));
                return c;
            }

            async Task<string> AskAsync(int tag, int wait = 5000) =>
                await LineAsync(await DialAsync($"opamp-raw tag={1000 + tag} ask={tag} wait={wait}"));

            async Task<byte[]> RestAsync(Stream from)
            {
                using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(20));
                using var all = new MemoryStream();
                await from.CopyToAsync(all, cts.Token);
                return all.ToArray();
            }

            // send, in pieces, then its side closed: the pattern, then the end.
            var c = await DialAsync("opamp-raw tag=1 send=1000 chunk=300 gap=1 end=fin");
            Assert.Equal(AdversaryServer.Body(1000), await RestAsync(c.GetStream()));

            // echo, and what it read: all of the pattern, then the peer's end.
            c = await DialAsync("opamp-raw tag=2 echo=1");
            await c.GetStream().WriteAsync(AdversaryServer.Body(5000));
            var back = new byte[5000];
            await c.GetStream().ReadExactlyAsync(back);
            Assert.Equal(AdversaryServer.Body(5000), back);
            c.Client.Shutdown(SocketShutdown.Send);
            Assert.Equal("fin 5000 0\n", await AskAsync(2));

            // Bytes that aren't the pattern's, and a reset.
            c = await DialAsync("opamp-raw tag=3");
            await c.GetStream().WriteAsync(new byte[] { 0, 1, 9 });
            await Task.Delay(100);
            c.Client.LingerState = new LingerOption(true, 0);
            c.Client.Close();
            Assert.Equal("reset 3 1\n", await AskAsync(3));

            // One still open is said to be, once the wait is over; one never seen is none.
            await DialAsync("opamp-raw tag=4");
            Assert.Equal("open 0 0\n", await AskAsync(4, wait: 200));
            Assert.Equal("none 0 0\n", await AskAsync(99));

            // A connection that never says who it is counts until it goes.
            var anon = new TcpClient();
            mine.Add(anon);
            await anon.ConnectAsync(IPAddress.Loopback, s.Port);
            Assert.Equal("open 1 0\n", await AskAsync(0, wait: 300));
            Assert.Equal(1, s.Open().Anonymous);
            anon.Close();
            Assert.Equal("none 0 0\n", await AskAsync(0));
            Assert.Equal(0, s.Open().Anonymous);

            // TLS after ok, under the certificate's name and the CA the client is given; then the
            // pattern inside it, and a close. Under another name the handshake fails.
            var options = ClientTransport.SslOptions(new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.Ca) });
            c = await DialAsync("opamp-raw tag=6 tls=1 send=100 end=close");
            using (var tls = new SslStream(c.GetStream(), leaveInnerStreamOpen: true))
            {
                options.TargetHost = TlsName;
                await tls.AuthenticateAsClientAsync(options);
                Assert.Equal(AdversaryServer.Body(100), await RestAsync(tls));
            }

            c = await DialAsync("opamp-raw tag=16 tls=1");
            using (var tls = new SslStream(c.GetStream(), leaveInnerStreamOpen: true))
            {
                options.TargetHost = "wrong.opamp-adversary.invalid";
                await Assert.ThrowsAsync<AuthenticationException>(() => tls.AuthenticateAsClientAsync(options));
            }

            // read=0 reads nothing until the run is released; delay=-1 never answers; reset is one.
            c = await DialAsync("opamp-raw tag=7 read=0");
            await c.GetStream().WriteAsync(AdversaryServer.Body(10));
            c.Client.Shutdown(SocketShutdown.Send);
            Assert.Equal("open 0 0\n", await AskAsync(7, wait: 200));
            s.Release();
            Assert.Equal("fin 10 0\n", await AskAsync(7));
            c = await DialAsync("opamp-raw tag=8 end=reset after=50");
            await Assert.ThrowsAnyAsync<IOException>(() => RestAsync(c.GetStream()));
            var silent = new TcpClient();
            mine.Add(silent);
            await silent.ConnectAsync(IPAddress.Loopback, s.Port);
            await silent.GetStream().WriteAsync(Encoding.ASCII.GetBytes("opamp-raw tag=9 delay=-1\n"));
            using (var quiet = new CancellationTokenSource(TimeSpan.FromMilliseconds(200)))
            {
                await Assert.ThrowsAnyAsync<OperationCanceledException>(() => silent.GetStream().ReadAsync(new byte[1], quiet.Token).AsTask());
            }

            // A first line that isn't the adversary's, and one too long: closed, unanswered.
            foreach (var line in new[] { "GET / HTTP/1.1\n", "opamp-raw tag=11 " + new string('x', 300) + "\n" })
            {
                var other = new TcpClient();
                mine.Add(other);
                await other.ConnectAsync(IPAddress.Loopback, s.Port);
                await other.GetStream().WriteAsync(Encoding.ASCII.GetBytes(line));
                byte[] answer;
                try
                {
                    answer = await RestAsync(other.GetStream());
                }
                catch (IOException)
                {
                    answer = []; // closed with what it sent unread: a reset
                }

                Assert.Empty(answer);
            }

            Assert.Equal("none 0 0\n", await AskAsync(11));

            // The marker: a connection accepted after it is late, one before it isn't.
            Assert.Contains("never got", s.AfterMark(), StringComparison.Ordinal);
            Assert.Null(await RawMark(s.Port));
            Assert.Null(s.AfterMark());
            await Task.Delay(2 * ArrivalGrace);
            await DialAsync("opamp-raw tag=10");
            Assert.Contains("RULE-SHELL-07", s.AfterMark(), StringComparison.Ordinal);
        }
        finally
        {
            foreach (var c in mine)
            {
                c.Dispose();
            }
        }
    }

    /// <summary>
    /// Open the harness's marker on the raw server at <paramref name="port"/>, as stop returns: the
    /// connection is made and its line written before this returns. The task gives what went
    /// wrong; null: the server said ok.
    /// </summary>
    private static Task<string?> RawMark(int port)
    {
        var c = new TcpClient { NoDelay = true };
        try
        {
            c.Connect(IPAddress.Loopback, port);
            c.GetStream().Write("opamp-raw mark=1\n"u8);
        }
        catch (Exception e) when (e is SocketException or IOException)
        {
            c.Dispose();
            return Task.FromResult<string?>(e.Message);
        }

        return RawMarkAnsweredAsync(c);
    }

    private static async Task<string?> RawMarkAnsweredAsync(TcpClient c)
    {
        using (c)
        {
            try
            {
                using var cts = new CancellationTokenSource(MarkBound);
                using var reader = new StreamReader(c.GetStream(), Encoding.ASCII);
                var answer = await reader.ReadLineAsync(cts.Token);
                return answer == "ok" ? null : $"the raw server answered the marker {answer ?? "nothing"}";
            }
            catch (OperationCanceledException)
            {
                return $"not answered within {MarkBound.TotalSeconds} s";
            }
            catch (Exception e) when (e is SocketException or IOException)
            {
                return e.Message;
            }
        }
    }

    /// <summary>One run's raw server.</summary>
    private sealed class AdversaryRaw : IDisposable
    {
        /// <summary>The longest first line the server reads.</summary>
        private const int MaxLine = 256;

        private readonly TcpListener listener = new(IPAddress.Loopback, 0);

        // What tls=1 starts TLS with (null: it can't).
        private readonly X509Certificate2? certificate;

        // Completed once the run's stop has returned: read=0 connections read on.
        private readonly TaskCompletionSource released = new(TaskCreationOptions.RunContinuationsAsynchronously);

        // Cancelled at teardown.
        private readonly CancellationTokenSource quit = new();
        private readonly object gate = new();

        // Every connection, but the harness's marker.
        private readonly HashSet<Conn> conns = new();

        // Those that said who they were.
        private readonly Dictionary<ulong, Conn> tags = new();

        // Completed, and made anew, each time a connection comes, says who it is or ends.
        private TaskCompletionSource changed = new(TaskCreationOptions.RunContinuationsAsynchronously);

        // When the marker's connection was accepted (a Stopwatch timestamp).
        private long? marked;

        /// <summary>Start a raw server on a port of its own.</summary>
        public AdversaryRaw(X509Certificate2? certificate)
        {
            this.certificate = certificate;
            this.listener.Start();
            _ = Task.Run(this.AcceptAsync);
        }

        public int Port => ((IPEndPoint)this.listener.LocalEndpoint).Port;

        /// <summary>
        /// Let the connections told not to read (read=0) read on: the run's stop has returned, and
        /// only a read tells the server a connection has gone.
        /// </summary>
        public void Release() => this.released.TrySetResult();

        /// <summary>End the server and every connection.</summary>
        public void Dispose()
        {
            this.Release();
            this.quit.Cancel();
            this.listener.Stop();
            Conn[] all;
            lock (this.gate)
            {
                all = [.. this.conns];
            }

            foreach (var rc in all)
            {
                rc.Socket.Dispose();
            }
        }

        /// <summary>The connections still open, and how many of them never said who they were.</summary>
        public (int All, int Anonymous) Open()
        {
            lock (this.gate)
            {
                var open = this.conns.Where(rc => rc.End is null).ToArray();
                return (open.Length, open.Count(rc => !rc.Said));
            }
        }

        /// <summary>
        /// What breaks RULE-SHELL-07 in when the run's connections were accepted (null: nothing
        /// does): one accepted more than <see cref="ArrivalGrace"/> after the marker's, which the
        /// harness opened the moment stop returned. (The server accepts in the order the
        /// connections were made.)
        /// </summary>
        public string? AfterMark()
        {
            lock (this.gate)
            {
                if (this.marked is not { } mark)
                {
                    return "the raw server never got the harness's marker";
                }

                foreach (var rc in this.conns)
                {
                    var late = Stopwatch.GetElapsedTime(mark, rc.Accepted);
                    if (late > ArrivalGrace)
                    {
                        return $"RULE-SHELL-07: a stream's connection reached the raw server {late.TotalMilliseconds:F0} ms after one the harness made as stop returned";
                    }
                }

                return null;
            }
        }

        /// <summary>
        /// Close <paramref name="socket"/> as a close is: what was sent goes, then the FIN. (.NET
        /// resets a socket closed with a read still under way, unless its sending side was shut
        /// down first.)
        /// </summary>
        private static void Close(Socket socket)
        {
            try
            {
                socket.Shutdown(SocketShutdown.Send);
            }
            catch (Exception e) when (e is SocketException or ObjectDisposedException)
            {
                // gone already
            }

            socket.Dispose();
        }

        /// <summary>Close <paramref name="socket"/> with a reset.</summary>
        private static void Reset(Socket socket)
        {
            try
            {
                socket.LingerState = new LingerOption(true, 0);
            }
            catch (Exception e) when (e is SocketException or ObjectDisposedException)
            {
                // gone already
            }

            socket.Dispose();
        }

        /// <summary>Whether <paramref name="e"/> is a connection going (or the server torn down) under a read or a write.</summary>
        private static bool Going(Exception e) =>
            e is IOException or SocketException or ObjectDisposedException or OperationCanceledException or AuthenticationException or InvalidOperationException;

        /// <summary>Change a connection under the lock, and wake whoever waits for one to say who it is or end.</summary>
        private void Note(Action change)
        {
            TaskCompletionSource woken;
            lock (this.gate)
            {
                change();
                woken = this.changed;
                this.changed = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            }

            woken.TrySetResult();
        }

        /// <summary>Wait <paramref name="ms"/>; false if the server is torn down first.</summary>
        private async Task<bool> SleepAsync(long ms)
        {
            if (ms <= 0)
            {
                return true;
            }

            try
            {
                await Task.Delay(TimeSpan.FromMilliseconds(Math.Min(ms, int.MaxValue)), this.quit.Token);
                return true;
            }
            catch (OperationCanceledException)
            {
                return false;
            }
        }

        private async Task AcceptAsync()
        {
            while (true)
            {
                Socket socket;
                try
                {
                    socket = await this.listener.AcceptSocketAsync();
                }
                catch (Exception e) when (e is SocketException or ObjectDisposedException or InvalidOperationException)
                {
                    return;
                }

                // Its run's from the moment it is accepted, whether or not it ever says who it is.
                var rc = new Conn(socket, Stopwatch.GetTimestamp());
                this.Note(() => this.conns.Add(rc));
                if (this.quit.IsCancellationRequested)
                {
                    socket.Dispose();
                }

                _ = Task.Run(() => this.ServeAsync(rc));
            }
        }

        /// <summary>The first line, without its newline (<see cref="MaxLine"/> bytes at most); null: none came.</summary>
        private async Task<string?> FirstLineAsync(Stream s)
        {
            var line = new List<byte>();
            var one = new byte[1];
            try
            {
                while (line.Count < MaxLine)
                {
                    if (await s.ReadAsync(one, this.quit.Token) == 0)
                    {
                        return null;
                    }

                    if (one[0] == '\n')
                    {
                        return Encoding.Latin1.GetString(line.ToArray());
                    }

                    line.Add(one[0]);
                }
            }
            catch (Exception e) when (Going(e))
            {
                // it went
            }

            return null;
        }

        private async Task ServeAsync(Conn rc)
        {
            var socket = rc.Socket;
            SslStream? ssl = null;
            try
            {
                socket.NoDelay = true;
                await using var tcp = new NetworkStream(socket, ownsSocket: false);
                var set = new Dictionary<string, string>();
                var line = await this.FirstLineAsync(tcp);
                var ours = line is not null && line.StartsWith("opamp-raw ", StringComparison.Ordinal);
                if (ours)
                {
                    foreach (var kv in line!["opamp-raw ".Length..].Split((char[]?)null, StringSplitOptions.RemoveEmptyEntries))
                    {
                        if (kv.IndexOf('=') is var eq and >= 0)
                        {
                            set[kv[..eq]] = kv[(eq + 1)..];
                        }
                    }
                }

                long Num(string name, long otherwise) =>
                    set.TryGetValue(name, out var v) && long.TryParse(v, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture, out var n) ? n : otherwise;

                if (ours && Num("mark", 0) == 1)
                {
                    // The harness's own, not the run's: when what was opened as stop returned got here.
                    this.Note(() =>
                    {
                        this.conns.Remove(rc);
                        this.marked = rc.Accepted;
                    });
                    await tcp.WriteAsync("ok\n"u8.ToArray(), this.quit.Token);
                    return;
                }

                void Ended(string how) => this.Note(() => rc.End ??= how);
                if (!ours)
                {
                    // It went, or said something else, before it said who it was.
                    Ended("closed");
                    return;
                }

                var tag = unchecked((ulong)Num("tag", 0));
                this.Note(() =>
                {
                    rc.Said = true;
                    this.tags[tag] = rc;
                });

                Stream rw = tcp;
                using var writing = new SemaphoreSlim(1, 1);
                async Task<bool> WriteAsync(ReadOnlyMemory<byte> b)
                {
                    await writing.WaitAsync();
                    try
                    {
                        await rw.WriteAsync(b, this.quit.Token);
                        return true;
                    }
                    catch (Exception e) when (Going(e))
                    {
                        return false;
                    }
                    finally
                    {
                        writing.Release();
                    }
                }

                var delay = Num("delay", 0);
                var held = delay < 0; // never answered: only read, until it goes
                if (!held)
                {
                    if (!await this.SleepAsync(delay) || !await WriteAsync("ok\n"u8.ToArray()))
                    {
                        Ended("closed");
                        return;
                    }

                    if (Num("tls", 0) == 1)
                    {
                        if (this.certificate is null)
                        {
                            Ended("closed");
                            return;
                        }

                        ssl = new SslStream(tcp, leaveInnerStreamOpen: true);
                        try
                        {
                            using var bound = CancellationTokenSource.CreateLinkedTokenSource(this.quit.Token);
                            bound.CancelAfter(TimeSpan.FromSeconds(30));
                            await ssl.AuthenticateAsServerAsync(new SslServerAuthenticationOptions { ServerCertificate = this.certificate }, bound.Token);
                        }
                        catch (Exception e) when (Going(e))
                        {
                            Ended("closed");
                            return;
                        }

                        rw = ssl;
                    }
                }

                // What the line asks the server to send, then how it ends the stream, beside the reading.
                var sent = Task.Run(async () =>
                {
                    if (held)
                    {
                        return;
                    }

                    if (set.TryGetValue("ask", out var about))
                    {
                        _ = ulong.TryParse(about, NumberStyles.None, CultureInfo.InvariantCulture, out var asked);
                        if (!await WriteAsync(Encoding.ASCII.GetBytes(await this.AnswerAsync(asked, Num("wait", 0)))))
                        {
                            return;
                        }
                    }

                    var body = AdversaryServer.Body(Math.Max(Num("send", 0), 0));
                    var chunk = Num("chunk", 0);
                    if (chunk <= 0)
                    {
                        chunk = Math.Max(body.Length, 1);
                    }

                    for (var off = 0L; off < body.Length; off += chunk)
                    {
                        if (off > 0 && !await this.SleepAsync(Num("gap", 0)))
                        {
                            return;
                        }

                        if (!await WriteAsync(body.AsMemory((int)off, (int)Math.Min(chunk, body.Length - off))))
                        {
                            return;
                        }
                    }

                    var end = set.GetValueOrDefault("end", string.Empty);
                    if (end is "" or "hold" || !await this.SleepAsync(Num("after", 0)))
                    {
                        return;
                    }

                    switch (end)
                    {
                        case "fin":
                            // Its side only: it still reads.
                            try
                            {
                                socket.Shutdown(SocketShutdown.Send);
                            }
                            catch (Exception e) when (Going(e))
                            {
                                // gone already
                            }

                            break;
                        case "close":
                            Ended("closed");
                            if (ssl is not null)
                            {
                                await writing.WaitAsync();
                                try
                                {
                                    await ssl.ShutdownAsync(); // TLS's close
                                }
                                catch (Exception e) when (Going(e))
                                {
                                    // gone already
                                }
                                finally
                                {
                                    writing.Release();
                                }
                            }

                            Close(socket);
                            break;
                        case "reset":
                            Ended("closed");
                            Reset(socket);
                            break;
                    }
                });

                if (Num("read", 1) == 0)
                {
                    // Reads nothing until the run is over.
                    await Task.WhenAny(this.released.Task, Task.Delay(Timeout.Infinite, this.quit.Token));
                }

                var echo = Num("echo", 0) == 1;
                var buf = new byte[64 << 10];
                while (true)
                {
                    int n;
                    try
                    {
                        n = await rw.ReadAsync(buf, this.quit.Token);
                    }
                    catch (Exception e) when (Going(e))
                    {
                        Ended("reset");
                        break;
                    }

                    if (n == 0)
                    {
                        Ended("fin");
                        break;
                    }

                    lock (this.gate)
                    {
                        for (var i = 0; i < n; i++)
                        {
                            if (buf[i] != (byte)((rc.Got + i) % 251))
                            {
                                rc.Bad = true;
                            }
                        }

                        rc.Got += n;
                    }

                    if (echo && !await WriteAsync(buf.AsMemory(0, n)))
                    {
                        Ended("reset");
                        break;
                    }
                }

                Close(socket);
                await sent;
            }
            catch (Exception e) when (Going(e))
            {
                // it went, or the server did
            }
            finally
            {
                this.Note(() => rc.End ??= "closed");
                Close(socket);
                ssl?.Dispose();
            }
        }

        /// <summary>
        /// Wait until connection <paramref name="tag"/> has ended (tag 0: until no connection that
        /// never said who it was is open), <paramref name="wait"/> ms at most, and say what the
        /// server knows of it: "STATE GOT BAD\n".
        /// </summary>
        private async Task<string> AnswerAsync(ulong tag, long wait)
        {
            Task deadline = Task.Delay(TimeSpan.FromMilliseconds(Math.Clamp(wait, 0, int.MaxValue)), this.quit.Token);
            while (true)
            {
                var (state, got, bad) = ("none", 0L, 0);
                Task changed;
                lock (this.gate)
                {
                    if (tag == 0)
                    {
                        got = this.conns.Count(rc => rc.End is null && !rc.Said);
                        state = got > 0 ? "open" : "none";
                    }
                    else if (this.tags.TryGetValue(tag, out var rc))
                    {
                        (state, got, bad) = (rc.End ?? "open", rc.Got, rc.Bad ? 1 : 0);
                    }

                    changed = this.changed.Task;
                }

                var answer = string.Create(CultureInfo.InvariantCulture, $"{state} {got} {bad}\n");
                if (state != "open" || await Task.WhenAny(changed, deadline) == deadline)
                {
                    return answer;
                }
            }
        }

        /// <summary>What the server knows of one connection.</summary>
        private sealed class Conn(Socket socket, long accepted)
        {
            public Socket Socket { get; } = socket;

            /// <summary>When it was accepted (a Stopwatch timestamp).</summary>
            public long Accepted { get; } = accepted;

            /// <summary>Its first line came.</summary>
            public bool Said { get; set; }

            /// <summary>Bytes read after the line.</summary>
            public long Got { get; set; }

            /// <summary>One of them wasn't the pattern's.</summary>
            public bool Bad { get; set; }

            /// <summary>null: open; fin: the peer closed its side; reset: it failed; closed: the server ended it.</summary>
            public string? End { get; set; }
        }
    }
}
