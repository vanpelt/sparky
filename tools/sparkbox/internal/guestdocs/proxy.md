# Sparkbox HTTPS proxy

Every sandbox has a default hostname under the Sparkbox domain. The edge
forwards requests to port `8000` in the VM unless the URL names another port or
the route is configured differently.

## Start a service

```sh
python3 -m http.server 8000 --bind 0.0.0.0
```

Bind to `0.0.0.0`, not `127.0.0.1`. The proxy reaches the service over the VM's
network interface.

## Other ports

Use the desired port in the public URL, for example
`https://your-box.<domain>:5173`, where `<domain>` is this deployment's own —
read it from `sparkbox whoami`'s `domain:` line rather than assuming one. The
development HTTPS ports exposed by the CKS edge are {{COMMON_HTTPS_PORTS}}.

To change where the default hostname forwards without putting a port in its
URL, run `sparkbox set-port PORT` inside the VM.

## Wake on request

A request to a paused sandbox asks Sparkbox to resume it before connecting.
Cold starts can make the first request slower than subsequent requests.

## Access

Every port is private by default and uses the Sparkbox browser session. Make a
port public only when the service on it should be reachable without your
account.

```sh
sparkbox make-public          # the default port only
sparkbox make-public 5173     # https://your-box.<domain>:5173
sparkbox make-private 5173    # close that one again
sparkbox make-private         # close every port
```

Visibility is per port, so opening one says nothing about the others: a public
preview on the default port leaves a debugger on `5173` gated. `make-public`
with no port therefore opens only the default port, while `make-private` with
no port closes all of them.

## WebSockets and streaming

The edge passes through WebSocket upgrades and streaming HTTP responses.
Application-level authentication, secrets, and authorization remain the
application's responsibility on public routes.
