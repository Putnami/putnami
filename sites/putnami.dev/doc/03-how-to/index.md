# How-To

How-To answers one question:
Can you build real things with Putnami, easily?

Each guide is:

- outcome-driven
- reassuring
- cross-layer
- minimal but complete

Each guide should feel like: "Oh. That's it?"

## What This Is Not

- not a framework manual
- not a CLI reference
- not exhaustive
- not language theory

If a guide tries to teach everything, it failed.

## By Intent

How-To is organized by user intent, not architecture.

### Plan

- [Write a feature spec](/docs/how-to/write-a-feature-spec)

### Build

- [Build a web app](/docs/how-to/build-a-web-app)
- [Build an API service](/docs/how-to/build-an-api-service)
- [Share code between projects](/docs/how-to/share-code-between-projects)
- [Configure your app](/docs/how-to/configure-your-app)

### Extend

- [Add persistence](/docs/how-to/add-persistence)
- [Add authentication](/docs/how-to/add-authentication)
- [Add background jobs](/docs/how-to/add-background-jobs)
- [Add observability](/docs/how-to/add-observability)
- [Add web analytics](/docs/how-to/add-web-analytics)
- [Structure business logic with DI](/docs/how-to/structure-business-logic-with-di)
- [Provision infrastructure and run conformance packs](/docs/how-to/provision-infra-and-run-conformance-packs)

### Ship

- [Qualify a workload](/docs/how-to/qualify-a-workload)
- [Gate production builds with the doctor preflight](/docs/how-to/gate-production-builds-with-doctor)
- [Publish impacted libraries coherently](/docs/how-to/publish-impacted-libraries)

### Maintain

- [Upgrade Putnami](/docs/how-to/upgrade-putnami)

## Learn by Running Code

If you prefer learning by running code, the TypeScript samples are a progressive learning path — 14 projects that build on each other from a minimal HTTP server to a full-stack app. Each sample is self-contained and runnable. They live in the repository under [`typescript/samples/`](https://github.com/putnami/putnami/tree/main/typescript/samples), alongside [`go/samples/`](https://github.com/putnami/putnami/tree/main/go/samples) and the experimental [`python/samples/`](https://github.com/putnami/putnami/tree/main/python/samples).

| How-To guide | Companion sample |
|---|---|
| [Build a web app](/docs/how-to/build-a-web-app) | [`03-web`](https://github.com/putnami/putnami/tree/main/typescript/samples/03-web) |
| [Build an API service](/docs/how-to/build-an-api-service) | [`02-rest-api`](https://github.com/putnami/putnami/tree/main/typescript/samples/02-rest-api) |
| [Share code between projects](/docs/how-to/share-code-between-projects) | [`05-dependency-injection`](https://github.com/putnami/putnami/tree/main/typescript/samples/05-dependency-injection) |
| [Configure your app](/docs/how-to/configure-your-app) | [`04-configuration`](https://github.com/putnami/putnami/tree/main/typescript/samples/04-configuration) |
| [Add persistence](/docs/how-to/add-persistence) | [`06-database`](https://github.com/putnami/putnami/tree/main/typescript/samples/06-database) |
| [Add authentication](/docs/how-to/add-authentication) | [`07-authentication`](https://github.com/putnami/putnami/tree/main/typescript/samples/07-authentication) |
| [Add web analytics](/docs/how-to/add-web-analytics) | [`13-fullstack-app`](https://github.com/putnami/putnami/tree/main/typescript/samples/13-fullstack-app) |
| [Structure business logic with DI](/docs/how-to/structure-business-logic-with-di) | [`05-dependency-injection`](https://github.com/putnami/putnami/tree/main/typescript/samples/05-dependency-injection) |
| [Extensions reference](/docs/tooling-&-workspace/extensions) | [`shell-extension`](https://github.com/putnami/putnami/tree/main/tooling/samples/shell-extension) |

The samples also cover surfaces the how-to guides do not: [real-time](https://github.com/putnami/putnami/tree/main/typescript/samples/08-real-time), [events](https://github.com/putnami/putnami/tree/main/typescript/samples/09-events), [service-to-service clients](https://github.com/putnami/putnami/tree/main/typescript/samples/10-service-to-service), [storage](https://github.com/putnami/putnami/tree/main/typescript/samples/11-storage), [caching](https://github.com/putnami/putnami/tree/main/typescript/samples/12-caching), [capabilities](https://github.com/putnami/putnami/tree/main/typescript/samples/14-capabilities), and a [capstone fullstack app](https://github.com/putnami/putnami/tree/main/typescript/samples/13-fullstack-app) that combines everything.

## Structure Rule

Every guide must say:

- what you'll build (first paragraph)
- what you'll end with (last paragraph)

And it must end with:

You now have X.
