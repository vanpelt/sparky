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
`https://your-box.catnip.sh:5173`. The development HTTPS ports exposed by the
CKS edge are {{COMMON_HTTPS_PORTS}}.

To change where the default hostname forwards without putting a port in its
URL, run `sparkbox set-port PORT` inside the VM.

## Wake on request

A request to a paused sandbox asks Sparkbox to resume it before connecting.
Cold starts can make the first request slower than subsequent requests.

## Access

Routes are private by default and use the Sparkbox browser session. Make a
route public only when the service should be reachable without your account.
Run `sparkbox make-public` or `sparkbox make-private` inside the VM to change
all of its routes together.

## WebSockets and streaming

The edge passes through WebSocket upgrades and streaming HTTP responses.
Application-level authentication, secrets, and authorization remain the
application's responsibility on public routes.
