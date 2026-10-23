# Source Provenance

WxBridge combines historical Go source from `Rprop/ipad`, commit
`0278be2947eef98cb73f46579bce4428f7e93b96`, with independently reconstructed
protocol integration and a new HTTP service.

The recovery target was an unstripped Go executable, module `xiawan/wx`, with
SHA-256 `3af46a902830de49c040eeb8d910de89ba3d27a81fd6724f1cbf48adea9a7e0b`.
The original executable is neither included nor executed by this project.
This repository is not a claim that the exact original Go source was recovered.

New integration/recovery code is in `internal/workflow`, `internal/httpapi`,
`cmd/wechat-flow`, `clientsdk/android/recovery_zt.go` and
`clientsdk/extinfo/recovery_ccd.go`, with focused tests alongside it.

`internal/workflow/target_assets/sae.pb` is an embedded protocol table recovered
from the target. Its SHA-256 is
`eb819dc4c677c5f4996aa8b4ecc0de33863e9f7f546a2b69b5ec92094cee79fb`.
It is static protocol material, not an account session or API key.

The working profile is iPad 8.0.75, with fresh QR confirmation, recovered
version-03 device-token/CCD data and hybrid secmanualauth. This successful
combination does not establish the minimum accepted client version or complete
equivalence to a current official client.

On 2026-10-23, authorized tests confirmed login, heartbeat, text send/receive,
image/sticker receipt, and AMR/MP4 playback. A custom GIF sent through the new
HTTP API received a server message ID; a repeated HTTP request returned the
stored result without another send. Automatic media downloading remains
unverified against actual incoming files. No account identifiers, chat
records, runtime session files or test credentials are shipped here.

Historical packages outside the new entry point remain reference material.
They are not all adapted or tested and are not exposed as API routes by
WxBridge. Real-time voice/video calls are outside this project's current scope.
