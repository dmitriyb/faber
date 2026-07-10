# NetworkBinding — the egress lock as a run-argv contribution

## What it is

The component that makes "the box has no route out" true. It never inspects,
filters, or proxies traffic itself: it contributes the docker-run flags and
environment that attach a step container to the workflow's internal network
and point every HTTP client at the user's allow-listing egress proxy. The
enforcement is topological — the `--internal` network has no route out, and
the dual-homed proxy is the only thing on it that also faces the world.

## Contribution (proxy mode, the default)

From the workflow-level `network:` section (`name`, `proxy`, `no_proxy`),
the binding emits:

- `--network <name>` — the named internal docker network. Faber requires the
  network to exist (preflight; it is created out-of-band with the user's
  companion services) and fails the step before launch if it does not.
- `-e HTTPS_PROXY=<proxy> -e HTTP_PROXY=<proxy>` — every well-behaved client
  in the box routes through the proxy.
- `-e NO_PROXY=<no_proxy joined with commas>` — exemptions for on-net
  companion services (the gateway, credential endpoints) plus
  `localhost,127.0.0.1`, so intra-network traffic never detours through the
  proxy.

The proxy itself — which destinations it allows, on which ports, with what
filtering — is a user companion service. Faber never reads the allow-list,
never validates it, and has no opinion about its contents; a box whose agent
needs an endpoint the proxy denies simply observes a refused CONNECT.
Companion services are brought up out-of-band (the user's compose file);
faber attaches ephemeral step containers and treats everything on the
network as opaque.

## Alternative mode: baked nftables

For users who reject the proxy dependency, the binding supports a
self-contained mode: the image carries an immutable firewall rule set loaded
by a root entrypoint at container start, which then drops privileges to the
non-root agent user. The binding's contribution shrinks to
`--cap-add NET_ADMIN` (the entrypoint needs it to load rules) and omits all
proxy environment; the rules are immutable to the agent because the agent
user cannot modify them and the loading happened before it existed. The two
modes are mutually exclusive per workflow — configuring both is a
validate-time error. The rule content is build-side user config (part of the
template's image recipe), not something this component generates.

## Offline dependencies corollary

An egress-locked box cannot fetch packages, modules, or models at run time.
Faber's answer is deliberately not a network feature: dependency stores are
pre-warmed outside the lock and arrive as ordinary user config — a cache
volume mount plus the toolchain's offline switches (e.g. a module proxy
disabled via env, checksum DB off, cache directory pointed at the volume).
The binding guarantees only that no route exists; making a toolchain happy
without one is the template author's job.

## Deferred seam: egress allow-list ownership

Today the allow-list lives entirely inside the user's proxy configuration,
invisible to faber. The open question (backlog) is whether templates should
declare endpoint *needs* — "this box talks to endpoint class X" — that faber
cross-checks against a proxy-published manifest at validate time, catching
"agent will hang on a denied endpoint" before launch. The first pass
reserves the seam and does nothing: faber stays blind to the allow-list, and
a denied endpoint surfaces as an ordinary in-box failure.

Requirements implemented: Network binding egress lock; Deferred: egress
allow-list ownership.
