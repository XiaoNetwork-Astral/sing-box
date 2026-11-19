!!! quote "Changes in sing-box 1.13.0"

    :material-plus: [quic_congestion_control](#quic_congestion_control)

### Structure

```json
{
"type": "naive",
"tag": "naive-in",
"network": "udp",
...
// Listen Fields

"users": [
{
"username": "sekai",
"password": "password"
}
],
"quic_congestion_control": "",
"fallback": {
"url": "http://127.0.0.1:8080",
"reject_url": "",
"reject_status_code": 0,
"headers": {},
"rewrite_host": false,
"enable_x_forwarded": false
},
"tls": {}
}
```

### Listen Fields

See [Listen Fields](/configuration/shared/listen/) for details.

### Fields

#### network

Listen network, one of `tcp` `udp`.

Both if empty.

#### users

==Required==

Naive users.

#### quic_congestion_control

!!! question "Since sing-box 1.13.0"

QUIC congestion control algorithm.

| Algorithm      | Description                     |
|----------------|---------------------------------|
| `bbr`          | BBR                             |
| `cubic`        | CUBIC                           |
| `reno`         | New Reno                        |

`bbr` is used by default.

#### fallback

Optional web fallback for requests that are not valid naive proxy requests.

Non-CONNECT requests are reverse-proxied to `url`, so the HTTPS port can behave like an ordinary website.
CONNECT requests that fail naive validation (missing padding or credentials) are never forwarded; they receive a configurable non-2xx response instead.

#### fallback.url

==Required==

Base URL of the local HTTP upstream, using `http` or `https`.

The request path and query are appended to the URL path and query.
For example, with `https://example.com/base`, a request for `/hello?q=1` is proxied to `/base/hello?q=1`.

#### fallback.reject_url

Optional page used for rejected CONNECT requests.

An absolute `http` or `https` URL is requested as-is.
A path starting with `/` is resolved against `fallback.url` the same way as a client request.

If empty, only `reject_status_code` is returned with an empty body.
If set, a new GET request is sent to the URL, and its response headers and body are returned with `reject_status_code` as the status.
The CONNECT target, body and credentials are never sent upstream; the `Host` of this request is the TLS server name of the client connection, or the host of `fallback.url` if there is none.
The response body is limited to 1 MiB; fetch failures, timeouts and oversized bodies also return `reject_status_code` with an empty body.

#### fallback.reject_status_code

Status code returned for rejected CONNECT requests.

`405` is used by default, including when set to `0`.
Other values must be between `400` and `599`.

#### fallback.headers

Headers set on every upstream request, overriding headers with the same name.

A `Host` entry replaces the default `Host` header.

#### fallback.rewrite_host

Use the host of the upstream URL as the `Host` header instead of the client's original `Host`.

Ignored when `fallback.headers` contains `Host`.

#### fallback.enable_x_forwarded

Send `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host` to the upstream.

The client IP is resolved automatically. A request is treated as relayed (for example by cloudflared) when the TCP peer is a loopback or private address, or the local address of the connection itself; all other requests are direct.

* Direct: the client IP is the TCP peer.
* Relayed: the client IP is taken from `Cf-Connecting-Ip`, otherwise from the right-most public address in `X-Forwarded-For`, otherwise the TCP peer.

`X-Forwarded-For` is set to that single address. A relayed request keeps a valid `X-Forwarded-Proto` and its `X-Forwarded-Host`; otherwise `X-Forwarded-Proto` is `https` when TLS is enabled for the inbound, `http` if not, and `X-Forwarded-Host` is the original `Host`.

Regardless of this option, `Forwarded`, `X-Forwarded-*`, `X-Real-Ip`, `Cf-Connecting-Ip` and `True-Client-Ip` sent by direct clients are removed, so they cannot be spoofed. When disabled, these headers of relayed requests are passed through unchanged.

#### tls

TLS configuration, see [TLS](/configuration/shared/tls/#inbound).