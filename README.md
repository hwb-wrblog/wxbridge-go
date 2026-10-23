# WxBridge

Go service for WeChat login, messaging and media APIs.

The service wraps an independently compiled protocol implementation. It does
not invoke the original WeChatPadPro executable or require its containers,
Redis, MySQL or an external WeChat gateway.

## Status

| Capability | Validation |
| --- | --- |
| QR login, heartbeat, text send/receive | Live-tested |
| Image and existing sticker sending | Live-tested with recipient confirmation |
| AMR voice and MP4 video sending | Recipient playback confirmed |
| Custom GIF upload/send | Real server acknowledgement; recipient animation not checked |
| Automatic received-media download | Implemented and tested offline; real receiving flow not yet verified |
| SILK voice sending | Container/chunk validation tested offline |
| Account identity, logout/switch, normalized messages and cumulative ACK | Implemented and tested offline |
| Real-time voice/video calls | Not implemented |

This is a recovered messaging core, not a complete implementation of all
WeChat APIs. It currently uses one tested iPad profile, 8.0.75 (`0x18004b2f`).
One process owns one account/state directory; multiple accounts require
separate processes and ports. See [PROVENANCE.md](PROVENANCE.md).

## Run a Deployment Package

Packages contain Linux amd64 and arm64 binaries, source and deployment files.
They contain no logged-in session, API token or chat history. Extract the
package on the target server, then run:

```sh
./start.sh
```

The default API address is `http://127.0.0.1:8787`. State is saved in `./state`.
The first start generates a private token at `state/api-token`. For calls
from another machine, choose the listener explicitly:

```sh
WECHAT_LISTEN=0.0.0.0:8787 ./start.sh
```

Every API request requires `Authorization: Bearer TOKEN`. Keep the token and
state directory private; put the API behind HTTPS when crossing networks.
The program does not configure your firewall or reverse proxy.

## Build From Source

Use Go 1.24 or newer to reproduce the tested build:

```sh
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o bin/wxbridge ./cmd/wechat-flow
./bin/wxbridge -state ./state serve -listen 127.0.0.1:8787
```

Create a portable deployment package:

```sh
./scripts/package.sh
```

The result is `dist/wxbridge-linux.tar.gz`. The recovered module name remains
`feiyu.com/wx` to preserve imports in the reference SDK; the project name is
WxBridge and the canonical repository is
<https://github.com/hwb-wrblog/wxbridge-go>.

## Connect a Project

See [API.md](API.md) for endpoints and examples, including QR login, media
uploads, sends, account switching and cursor-based message polling/acknowledgement. A fetch-based JavaScript
client is in [examples/client.mjs](examples/client.mjs).

Send operations require a unique `Idempotency-Key`. Reusing the same key and
request returns the original result without sending again. An interrupted
request stays marked as unknown after restart; it is never silently retried.
Message IDs are JSON strings to preserve 64-bit precision.

`GET /v1/status` identifies the account with `wxid`, `nickname` and `avatar_url`.
`POST /v1/logout` clears the account before a new QR login. Message polling
provides normalized conversation/sender IDs, direction, group and mention flags.
One consumer should persist unfiltered pages, then acknowledge `next_cursor`
through `POST /v1/messages/ack`; acknowledged bodies are removed from memory
and the session file. Cursors remain stable across cleanup and restart.
Incoming media must be downloaded or explicitly discarded before ACK. Media
caches, uploads, logs and send receipts have separate disk-retention needs.

The service synchronizes messages in the background. Automatic media download
starts with incoming messages received after the first API startup; existing
saved messages can be downloaded explicitly. Completed files have size and
SHA-256 metadata and are served only after validation. Failed downloads are
recorded, retried at most three times automatically, and remain inspectable.
No webhook integration is included in this version.

## Deployment Options

- [compose.yaml](compose.yaml): persistent state volume; loopback publishing by default.
- [deploy/wxbridge.service](deploy/wxbridge.service): systemd example using a dynamic user and `/var/lib/wxbridge` state.
- `WECHAT_STATE_DIR`: override the state directory used by `start.sh`.
- `WECHAT_AUTO_DOWNLOAD=false`: disable automatic downloads while retaining explicit download requests.

For systemd, extract the package to `/opt/wxbridge`, then:

```sh
sudo cp deploy/wxbridge.service /etc/systemd/system/wxbridge.service
sudo systemctl daemon-reload
sudo systemctl enable --now wxbridge
```

Do not run the CLI and HTTP service simultaneously against the same state
directory. The exclusive state lock deliberately prevents this.

## Checks

```sh
go test -race -count=1 -buildvcs=false ./internal/workflow ./internal/httpapi ./cmd/wechat-flow ./clientsdk/extinfo
go vet -buildvcs=false ./internal/workflow ./internal/httpapi ./cmd/wechat-flow ./clientsdk/extinfo
```

These focused checks run offline. Legacy packages retained from the reference
source are not a supported server entry point.
