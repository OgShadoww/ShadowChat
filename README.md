# ShadowChat v1

A small, standard-library-only Go terminal chat for people on the same local
network. One computer hosts one room; every participant connects directly to
that host over TLS. The host starts an ordinary local client so its owner can
chat too. Different ports can run separate rooms.

## Build and try it locally

Install Go 1.22 or newer:

```sh
go build -o shadowchat .
```

In terminal 1:

```sh
./shadowchat host --listen 127.0.0.1:9000 --name Orest
```

Copy the printed 64-character TLS fingerprint. In terminal 2, replace
`HOST_FINGERPRINT` with that value:

```sh
./shadowchat join --address 127.0.0.1:9000 --name Sophia --fingerprint HOST_FINGERPRINT
```

Type a message and press Enter. Both terminals receive it, including the sender:

```text
[14:32:08] Sophia: Hello!
```

`/help` shows help; `/quit` leaves. Ctrl+C or end of terminal input also exits.
When the host exits, the whole room ends and clients report the disconnection.
Incoming messages can interrupt the typing line. Times use the receiver's local
timezone (UTC in the minimal Docker image).

Nicknames are case-sensitive, unique within the room, and contain 1–24 ASCII
letters, digits, underscores, or hyphens. Messages must be nonblank and no more
than 2,000 UTF-8 bytes, so some characters use multiple bytes. Terminal control
characters are displayed as spaces.

## Two computers on a LAN

On the host:

```sh
./shadowchat host --listen 0.0.0.0:9000 --name Orest
```

Share the fingerprint directly with your friend through a trusted channel,
such as showing your screen or an already trusted conversation. They must copy
that exact value into `--fingerprint`; a mismatch prevents the nickname and
messages from being sent. **The fingerprint changes every host restart.**
Confirm the new value with the host rather than trusting one shown by a failed
connection attempt.

On the other computer, substitute the host's actual LAN IP and fingerprint:

```sh
./shadowchat join --address 192.168.1.50:9000 --name Sophia --fingerprint HOST_FINGERPRINT
```

`0.0.0.0` is a listen address, not the address friends should connect to. Find
the host's LAN IP in its network settings. Guest Wi-Fi client isolation or a
host firewall may prevent connections; the host must allow incoming TCP on the
chosen port. No discovery, port forwarding, or network-interface detection is
included.

## Docker

```sh
docker build -t shadowchat .
docker run --rm -it -p 9000:9000 shadowchat host --listen 0.0.0.0:9000 --name Orest
```

Use `-it` because the host participates through its terminal. Remote clients
connect to the Docker host computer's LAN IP on port 9000, using the fingerprint
printed in the container. The multi-stage image contains a static binary and
runs as non-root UID/GID 65532. It does not persist certificates or messages.

## Protocol and code walkthrough

Each wire frame is one JSON object followed by a newline, over TCP/TLS. For
example, a client sends:

```json
{"type":"hello","name":"Sophia"}
{"type":"chat","text":"Hello!"}
```

The server replies and broadcasts:

```json
{"type":"welcome","name":"Sophia"}
{"type":"notice","text":"Sophia joined"}
{"type":"chat","name":"Sophia","text":"Hello!","time":"2026-09-27T14:32:08Z"}
{"type":"notice","text":"Sophia left"}
{"type":"error","text":"Nickname already in use"}
```

Hello is required first. The server associates the accepted nickname with the
connection and assigns timestamps; later client-supplied names/times are
ignored. Unknown types and invalid chat text receive errors. Invalid hello,
malformed JSON, and oversized frames receive an error and close the connection.
Frames are capped at 16 KiB **including the newline**, before JSON parsing.
JSON escaping keeps embedded newlines inside a single frame. Buffered reads
handle fragmented and combined TCP data.

| File | Responsibility |
| --- | --- |
| `main.go` | Flags, signal handling, host startup, printed connection details. |
| `server.go` | Accept connections, manage the room, read/validate messages, queue writes, shut down. |
| `client.go` | Pinned TLS dialing, terminal input, `/help` and `/quit`, message display. |
| `protocol.go` | Message shape, bounded frame reads, nickname/text validation, safe terminal text. |
| `tls.go` | Ephemeral self-signed certificate and mandatory SHA-256 certificate pinning. |
| `server_test.go` | Local TLS integration tests and bounded-queue/input tests. |

A message travels from the sender's terminal to its writer queue, through TLS
to the server's connection reader, then through a channel to the room goroutine.
The room creates the trusted sender/timestamp and queues a copy for every
participant, including the sender. Each connection's writer serializes that
message as newline-delimited JSON; receiving clients print it.

The room goroutine alone owns the nickname map. Readers send join, leave, and
chat events to it; join replies use a buffered response channel. Each connection
has one reader and one writer, with a 16-message outgoing queue. Only the writer
writes application messages. A full queue disconnects that client without
blocking the room. There are at most 32 server connections, including the host's
local client and pending handshakes. Excess connections are closed.

TLS handshakes, the initial hello/welcome, and writes each have a five-second
deadline. Established clients have no idle read deadline. A shared cancellation
signal closes the listener and active/pending connections; the server waits for
its goroutines to finish. A per-connection `sync.Once` closes the transport and
shutdown channel safely. Outgoing channels are never closed, avoiding concurrent
send/close races. Closing the transport directly also prevents a TLS close
notification from blocking room eviction.

The terminal uses a single process-lifetime input goroutine. Portable standard
library reads from a terminal cannot always be cancelled, so it may stay blocked
until the process exits; it cannot keep the app running after disconnection.
Network reader/writer goroutines are explicitly stopped and joined.

## Security and v1 limits

- TLS 1.2 or newer protects client-to-host traffic. Each startup generates an
  ephemeral ECDSA certificate, kept only in memory, and prints SHA-256 over its
  DER bytes. Clients compare this hash in mandatory `VerifyConnection` before
  sending hello, including the host's own client. `InsecureSkipVerify` disables
  public-CA/hostname checks only because this exact certificate pin replaces
  them. Trust is based on the pin, not the certificate's dates or DNS names.
- The host can read and relay every message. This is **not** anonymity or
  encryption that hides messages from the host.
- Anyone with network access and the fingerprint can join. A certificate
  fingerprint identifies the host certificate; it is not a secret password or
  client authentication. Nicknames are not verified identities.
- No message storage or replay: messages exist only in memory while being
  delivered. A newly joined client receives no earlier conversation.
- No reconnection, failover, peer mesh, multiple named rooms, or delivery
  acknowledgements. Queued messages can be lost on disconnect. A silent broken
  network may take time for TCP to detect; there is no application heartbeat.
- Bounded frames, queues, deadlines, and connections keep resource use small,
  but there is no rate limiting or protection against deliberate room flooding.
  The 32-connection cap is a limit, not a measured performance claim.

## Checks

```sh
gofmt -w *.go
go vet ./...
go test ./...
go test -race ./...
```

Tests use local connections, protocol events, and bounded timeouts, with no
arbitrary sleeps. They cover message exchange, sender identity, duplicate names,
departures, split/combined frames, malformed/oversized frames, incorrect pins,
validation, shutdown, slow-client eviction, and terminal input recovery.
