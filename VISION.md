# Creo — Vision

*The North Star. Deliberately short; if a decision contradicts this document, one of the two is wrong and we say so out loud.*

## The one sentence

**Anyone can create and maintain real software by describing what they want — and truly own the result, on infrastructure they control.**

## The world we're building toward

- **Creation is a conversation.** A bakery owner, a kid, a club organizer builds and evolves working software the way they'd brief a trusted craftsperson: describe, look, point, refine. They never see code, terminals, or stack traces — not because code is hidden behind a curtain, but because the product genuinely speaks their language.
- **Ownership is real.** Your projects live on infrastructure you (or someone you trust) control. You can run the whole platform on a laptop, a home server, or a cluster. You can export everything, always. Nothing about the design makes you a hostage — not to us, not to a cloud, not to a model provider.
- **Projects outlive everything around them.** Devices die, browsers close, servers restart, model providers come and go, better models replace worse ones. The project — its history, its versions, its live site — persists through all of it. Durability is the product's deepest promise.
- **One substrate, many products.** The same core powers a website builder for non-coders, a game builder for kids, and products we haven't imagined — each an opinionated experience defined as configuration, not a fork. Eventually, people who aren't us build verticals on it.
- **Safe by architecture, not by promise.** Generated code is assumed hostile; credentials are structurally unreachable from it; tenants are isolated by construction. Trust claims are documented per deployment tier and never overstated.

## What Creo is at maturity

An **open-source, self-hostable agent platform** (the substrate: durable sessions, versioned artifacts, sandboxed execution, provider-agnostic models, publishing) plus a **family of commercial verticals** built on it — ours first, an ecosystem's later. The same binary a hobbyist runs at home is the same core a hosted service scales to millions.

## What we refuse to become

- A developer tool with the files merely hidden — if success requires the user to understand the implementation, we failed.
- A template engine with a chatbot — a fixed schema of what users may build caps their imagination at our own; the model writes real code precisely so "make it look like a hand-drawn zine" works.
- Cloud-only, or provider-locked — either would break the ownership promise.
- A platform that overstates its safety — "mostly isolated" is a creative spelling of "breached."

## Horizons

1. **H1 — prove it:** the websites vertical, self-hosted on one machine, end-to-end: describe → preview → refine → publish → return next month from another device and continue.
2. **H2 — earn trust at scale:** more verticals (kids' games next), a hosted offering, hardened multi-tenancy, the cluster profile.
3. **H3 — become soil:** third parties build verticals on the open core; Creo is infrastructure other products stand on.

## How we'll know it's working

A non-coder maintains a site they're proud of for years without ever seeing code. A developer builds a working vertical from the docs alone, without talking to us. The same project survives a laptop → server → cloud migration with its full history. And the honest numbers stay honest: zero committed-event loss, zero cross-tenant incidents, ever.
