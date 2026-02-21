# vastproxy

## TL;DR

This is a proxy server that can be used to treat multiple vast.ai SGLang images
as if they are one logical API instance:

![Screenshot](https://raw.githubusercontent.com/shutej/vastproxy/master/screenshot.png)

## Installation

```console
$ go install github.com/shutej/vastproxy@latest
```

## Configuration

Copy [.env.example](.env.example) to `.env` and add your details. You'll need
an API key (with read/write abilities, except Billing/Earnings) and an SSH key
registered with Vast.

## Details

- [x] Discovers and auto-enrolling instances automatically with the Vast API
      with no manual configuration.
- [x] Rigorous health and lifecycle checks using nvidia-smi.  Shows fast direct
      SSH connections or slow proxied SSH connections.
- [x] SSH tunneling for strong transport security; you can remove all Docker
      ports from your template, the SGLang HTTP interface is tunnel-only.
- [x] Sticky routing: if you propagate the `X-VastProxy-Instance` response
      header to subsequent requests they'll route to the same instance,
      allowing stateless APIs to benefit from KV caching.
- [x] Bidirectional visibility: proxied instances are labeled as such in the
      Vast UI.
