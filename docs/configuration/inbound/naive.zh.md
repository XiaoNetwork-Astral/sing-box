!!! quote "sing-box 1.13.0 中的更改"

    :material-plus: [quic_congestion_control](#quic_congestion_control)

### 结构

```json
{
"type": "naive",
"tag": "naive-in",
"network": "udp",

... // 监听字段

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

### 监听字段

参阅 [监听字段](/zh/configuration/shared/listen/)。

### 字段

#### network

监听的网络协议，`tcp` `udp` 之一。

默认所有。

#### users

==必填==

Naive 用户。

#### quic_congestion_control

!!! question "Since sing-box 1.13.0"

QUIC 拥塞控制算法。

| 算法             | 描述                 |
|----------------|--------------------|
| `bbr`          | BBR                |
| `cubic`        | CUBIC              |
| `reno`         | New Reno           |

默认使用 `bbr`。

#### fallback

可选的 Web 回退，用于处理不是有效 naive 代理请求的请求。

非 CONNECT 请求会被反向代理到 `url`，使 HTTPS 端口表现得像一个普通网站。
认证失败或缺少 naive 填充头的 CONNECT 请求不会被转发，而是返回可配置的非 2xx 响应。

#### fallback.url

==必填==

本地 HTTP 上游的基础 URL，使用 `http` 或 `https`。

请求的路径和查询参数会追加到 URL 的路径和查询参数之后。
例如，使用 `https://example.com/base` 时，对 `/hello?q=1` 的请求会被代理到 `/base/hello?q=1`。

#### fallback.reject_url

用于被拒绝的 CONNECT 请求的可选页面。

绝对 `http` 或 `https` URL 会按原样请求。
以 `/` 开头的路径会像客户端请求一样，基于 `fallback.url` 解析。

为空时，仅返回 `reject_status_code` 和空响应体。
设置后，会向该 URL 发送新的 GET 请求，并将其响应头和响应体连同 `reject_status_code` 状态码一起返回。
原 CONNECT 的目标地址、请求体和认证信息不会发给上游；这个请求的 `Host` 使用客户端 TLS 握手中的服务器名称，没有时使用 `fallback.url` 的主机。
响应体限制为 1 MiB；请求失败、超时或响应体过大时，同样返回 `reject_status_code` 和空响应体。

#### fallback.reject_status_code

被拒绝的 CONNECT 请求返回的状态码。

默认使用 `405`，设置为 `0` 时也是如此。
其他值必须在 `400` 到 `599` 之间。

#### fallback.headers

在每个上游请求中设置的请求头，会覆盖同名的请求头。

`Host` 条目会替换默认的 `Host` 请求头。

#### fallback.rewrite_host

使用上游 URL 的主机名作为 `Host` 请求头，而不是客户端原始的 `Host`。

当 `fallback.headers` 包含 `Host` 时忽略此项。

#### fallback.enable_x_forwarded

向上游发送 `X-Forwarded-For`、`X-Forwarded-Proto` 和 `X-Forwarded-Host`。

客户端 IP 自动判断。TCP 对端是回环地址、内网地址，或者就是本次连接的本地地址时，请求视为经过中转（例如 cloudflared）；其余请求视为直连。

* 直连：客户端 IP 为 TCP 对端地址。
* 中转：优先取 `Cf-Connecting-Ip`，其次取 `X-Forwarded-For` 中最右侧的公网地址，都没有时使用 TCP 对端地址。

`X-Forwarded-For` 只包含这一个地址。中转请求已带合法的 `X-Forwarded-Proto` 和 `X-Forwarded-Host` 时沿用；否则入站启用 TLS 时 `X-Forwarded-Proto` 为 `https`，未启用时为 `http`，`X-Forwarded-Host` 为原始 `Host`。

无论是否启用，直连客户端自带的 `Forwarded`、`X-Forwarded-*`、`X-Real-Ip`、`Cf-Connecting-Ip` 和 `True-Client-Ip` 都会被移除，无法伪造。禁用时，中转请求的这些请求头原样传递。

#### tls

TLS 配置, 参阅 [TLS](/zh/configuration/shared/tls/#入站)。