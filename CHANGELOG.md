# Changelog

All notable changes to mamori are documented here. This file is generated from Conventional Commits by semantic-release.

## [1.12.2](https://github.com/xavidop/mamori/compare/v1.12.1...v1.12.2) (2026-08-24)


### Bug Fixes

* ci ([f79f509](https://github.com/xavidop/mamori/commit/f79f50939d0491278577a8e8ea961fdd29f427c4))

## [1.12.1](https://github.com/xavidop/mamori/compare/v1.12.0...v1.12.1) (2026-08-06)


### Bug Fixes

* force publish ([89b1624](https://github.com/xavidop/mamori/commit/89b16245216609a011f42072a714002760d47bb5))

# [1.12.0](https://github.com/xavidop/mamori/compare/v1.11.1...v1.12.0) (2026-08-06)


### Features

* provider Close lifecycle contract, and flatten:"toml" ([#149](https://github.com/xavidop/mamori/issues/149)) ([e4f6b05](https://github.com/xavidop/mamori/commit/e4f6b05c76971f20e344f1a88fcec0dd7256abfb))

## [1.11.1](https://github.com/xavidop/mamori/compare/v1.11.0...v1.11.1) (2026-08-04)


### Bug Fixes

* **site:** lay the five idea cards out as three and two, not four and one ([#148](https://github.com/xavidop/mamori/issues/148)) ([7536940](https://github.com/xavidop/mamori/commit/7536940ec0a155b8e88efa64f7eb738d78e9d7de))

# [1.11.0](https://github.com/xavidop/mamori/compare/v1.10.0...v1.11.0) (2026-08-04)


### Features

* **core:** WithBootstrapCache, so a restart survives a backend outage ([#147](https://github.com/xavidop/mamori/issues/147)) ([1b30f39](https://github.com/xavidop/mamori/commit/1b30f3984c53086d04b032e33da552b336953ef2))

# [1.10.0](https://github.com/xavidop/mamori/compare/v1.9.1...v1.10.0) (2026-08-04)


### Features

* **httpcore:** bounded SSE framing, and migrate the two streaming providers ([#146](https://github.com/xavidop/mamori/issues/146)) ([37f64e3](https://github.com/xavidop/mamori/commit/37f64e337ed45a4b98e9017dc3716d3036a9d1a7))

## [1.9.1](https://github.com/xavidop/mamori/compare/v1.9.0...v1.9.1) (2026-08-04)


### Bug Fixes

* restore the provider registrations lost in the merge cascade ([#142](https://github.com/xavidop/mamori/issues/142)) ([889bda1](https://github.com/xavidop/mamori/commit/889bda17d0a9f54a273ae92bf81b713b3e4f31bb))

# [1.9.0](https://github.com/xavidop/mamori/compare/v1.8.0...v1.9.0) (2026-08-04)


### Features

* **bitwarden:** add Bitwarden Secrets Manager provider ([#138](https://github.com/xavidop/mamori/issues/138)) ([bba9576](https://github.com/xavidop/mamori/commit/bba95762af59555c6132eb72aae740108fa20a28))
* **hcp-vault-secrets:** resolve secrets from HCP Vault Secrets over httpcore ([#135](https://github.com/xavidop/mamori/issues/135)) ([babae89](https://github.com/xavidop/mamori/commit/babae8987d1720867ae90376d18c93a5bbfd2a68))
* **heroku:** resolve Heroku config vars in one request per app ([#136](https://github.com/xavidop/mamori/issues/136)) ([38482f7](https://github.com/xavidop/mamori/commit/38482f743699e761739216eb51db2ac051dbac47))
* **infisical:** resolve secrets from Infisical over httpcore ([#133](https://github.com/xavidop/mamori/issues/133)) ([c68e593](https://github.com/xavidop/mamori/commit/c68e593a1ad074305e1f1074eb733ed14bbae153))
* **nacos:** Alibaba Nacos provider with a native long-poll watch ([#139](https://github.com/xavidop/mamori/issues/139)) ([d51bb87](https://github.com/xavidop/mamori/commit/d51bb87ca9e093fbe22e3b74956c4ce79f97b4b5))
* **posthog:** resolve PostHog feature flags over the v2 flags endpoint ([#134](https://github.com/xavidop/mamori/issues/134)) ([161b598](https://github.com/xavidop/mamori/commit/161b598c4d0db06cb2496ef76559e28259f8f28d))
* **supabase:** resolve Supabase Vault secrets over PostgREST ([#137](https://github.com/xavidop/mamori/issues/137)) ([ba3cc55](https://github.com/xavidop/mamori/commit/ba3cc55fd39d98a6b9866c72ea46d58c44d26791))

# [1.8.0](https://github.com/xavidop/mamori/compare/v1.7.0...v1.8.0) (2026-08-04)


### Features

* **httpcore,https:** a shared HTTP resolve core, and a generic https:// provider on it ([#117](https://github.com/xavidop/mamori/issues/117)) ([de594e0](https://github.com/xavidop/mamori/commit/de594e04d34654ac2b911f3094a89099b7a3ae50)), closes [#107](https://github.com/xavidop/mamori/issues/107)

# [1.7.0](https://github.com/xavidop/mamori/compare/v1.6.2...v1.7.0) (2026-08-03)


### Features

* **core:** WithDerive, computed fields that stay correct across rotation ([#112](https://github.com/xavidop/mamori/issues/112)) ([d066a37](https://github.com/xavidop/mamori/commit/d066a37a9166c7b48f587b2fc4869a2fbf9dc074))

## [1.6.2](https://github.com/xavidop/mamori/compare/v1.6.1...v1.6.2) (2026-08-01)


### Bug Fixes

* **providers:** bound and drain HTTP error bodies, and pin the guards that were unpinned ([#115](https://github.com/xavidop/mamori/issues/115)) ([1790952](https://github.com/xavidop/mamori/commit/179095248b4ce39d254519883a18d135a96d7492)), closes [#103](https://github.com/xavidop/mamori/issues/103) [#104](https://github.com/xavidop/mamori/issues/104)

## [1.6.1](https://github.com/xavidop/mamori/compare/v1.6.0...v1.6.1) (2026-08-01)


### Bug Fixes

* fail Watch loudly on an OnChange hook typed for the wrong T ([#114](https://github.com/xavidop/mamori/issues/114)) ([e86491a](https://github.com/xavidop/mamori/commit/e86491af74bc333592cf9896926bae28533270fd))

# [1.6.0](https://github.com/xavidop/mamori/compare/v1.5.1...v1.6.0) (2026-07-31)


### Features

* **cloudflare-kv:** Cloudflare Workers KV provider ([#104](https://github.com/xavidop/mamori/issues/104)) ([f51222a](https://github.com/xavidop/mamori/commit/f51222a499dc83d9902fb5a23fe93cb5e9aac509)), closes [#selector](https://github.com/xavidop/mamori/issues/selector) [#fields](https://github.com/xavidop/mamori/issues/fields)
* **scaleway-sm:** Scaleway Secret Manager provider ([#106](https://github.com/xavidop/mamori/issues/106)) ([42053a0](https://github.com/xavidop/mamori/commit/42053a0d4f482876e2097d654841d727c25a5793))
* **vercel-gc:** Vercel Global Config provider ([#103](https://github.com/xavidop/mamori/issues/103)) ([6891410](https://github.com/xavidop/mamori/commit/6891410c6d63926ddf103838a1d6cb6d44a4e256)), closes [#field](https://github.com/xavidop/mamori/issues/field) [/cfg#field](https://github.com//cfg/issues/field)

## [1.5.1](https://github.com/xavidop/mamori/compare/v1.5.0...v1.5.1) (2026-07-30)


### Bug Fixes

* **docs:** highlight the clicked section in the on-this-page TOC ([#102](https://github.com/xavidop/mamori/issues/102)) ([7c0ea95](https://github.com/xavidop/mamori/commit/7c0ea95aa0cb7a6b4415bf8849d18cff15603880))

# [1.5.0](https://github.com/xavidop/mamori/compare/v1.4.0...v1.5.0) (2026-07-30)


### Features

* **cli:** mamori diff, a config-surface and privilege delta for PR review ([#97](https://github.com/xavidop/mamori/issues/97)) ([6caaa58](https://github.com/xavidop/mamori/commit/6caaa581be8827ff3f46153709daf7bbaf44fd35))

# [1.4.0](https://github.com/xavidop/mamori/compare/v1.3.3...v1.4.0) (2026-07-30)


### Bug Fixes

* let an exec: argument contain a space, and document env vars ([#74](https://github.com/xavidop/mamori/issues/74)) ([d38bd40](https://github.com/xavidop/mamori/commit/d38bd403418457c73e812bd7e1d1f32d8ac3de06))


### Features

* ?decode= value transforms ([#57](https://github.com/xavidop/mamori/issues/57)) ([63571f8](https://github.com/xavidop/mamori/commit/63571f8e85f7175636a08f0214196270d464db2c)), closes [#56](https://github.com/xavidop/mamori/issues/56)
* ${VAR} ref interpolation via WithRefVars ([#60](https://github.com/xavidop/mamori/issues/60)) ([43ed2e8](https://github.com/xavidop/mamori/commit/43ed2e88533f1e90cab6cf6cf76c34ff0cf2cb76))
* **aws:** aws-appconfig provider ([#66](https://github.com/xavidop/mamori/issues/66)) ([2c68e40](https://github.com/xavidop/mamori/commit/2c68e400daa0a91be5179f98e7d513bbddc80a56))
* **azure:** azure-appconfig provider ([#67](https://github.com/xavidop/mamori/issues/67)) ([c109c58](https://github.com/xavidop/mamori/commit/c109c58e4e5a67d2f63c35b31c1db7fafd5b2d99))
* Meter counters for stale, dropped and rejected, plus the x/prom bridge ([#73](https://github.com/xavidop/mamori/issues/73)) ([2d1383e](https://github.com/xavidop/mamori/commit/2d1383e15d4d983cfe4e582787408e2d1aca1418)), closes [#key](https://github.com/xavidop/mamori/issues/key)
* **openfeature:** openfeature provider ([#68](https://github.com/xavidop/mamori/issues/68)) ([f025615](https://github.com/xavidop/mamori/commit/f0256155ff0c7493e8849e19775f6fa3bd09b233))
* PreApply gate for rotation safety ([#61](https://github.com/xavidop/mamori/issues/61)) ([4ddbff2](https://github.com/xavidop/mamori/commit/4ddbff23adb98b85897eac3586d2604094ea32d9))
* Refresh forces an immediate re-resolve ([#62](https://github.com/xavidop/mamori/issues/62)) ([74edb00](https://github.com/xavidop/mamori/commit/74edb00d35ce95b7171b1891e996ff651304eaa1))
* RFC 6901 JSON Pointer nested key selection ([#53](https://github.com/xavidop/mamori/issues/53)) ([33bcdb8](https://github.com/xavidop/mamori/commit/33bcdb8b94118d2b167651e1a6393f196ab9df14)), closes [#fragment](https://github.com/xavidop/mamori/issues/fragment) [#value](https://github.com/xavidop/mamori/issues/value) [#fragment](https://github.com/xavidop/mamori/issues/fragment) [#fragment](https://github.com/xavidop/mamori/issues/fragment) [#value](https://github.com/xavidop/mamori/issues/value)
* **secret:** add Clone, and stop Zero's doc recommending corruption ([#72](https://github.com/xavidop/mamori/issues/72)) ([a07d508](https://github.com/xavidop/mamori/commit/a07d5083fe235ae09ab9946d0d58a256fbfbb31a))
* structured engine logging via WithLogger ([#70](https://github.com/xavidop/mamori/issues/70)) ([c97fe2e](https://github.com/xavidop/mamori/commit/c97fe2e0a076ffd4e50421367d93d33dcd7336b5))
* **viper:** viper provider ([#69](https://github.com/xavidop/mamori/issues/69)) ([8413a62](https://github.com/xavidop/mamori/commit/8413a6285293084827f245ae587e490131f3ac3a))

## [1.3.3](https://github.com/xavidop/mamori/compare/v1.3.2...v1.3.3) (2026-07-29)


### Bug Fixes

* make WithBackoff actually do something ([#63](https://github.com/xavidop/mamori/issues/63)) ([b6c1d61](https://github.com/xavidop/mamori/commit/b6c1d61941775ca65ab92a13b6e7ca9fa5f6f6f4))

## [1.3.2](https://github.com/xavidop/mamori/compare/v1.3.1...v1.3.2) (2026-07-28)


### Bug Fixes

* **server:** unlink the Unix socket before draining, not after ([#58](https://github.com/xavidop/mamori/issues/58)) ([5aa2c87](https://github.com/xavidop/mamori/commit/5aa2c87d47c1a93e0589b09bcc7ce61452876181))

## [1.3.1](https://github.com/xavidop/mamori/compare/v1.3.0...v1.3.1) (2026-07-27)


### Bug Fixes

* **server:** Close/Serve data race on listenWG ([#55](https://github.com/xavidop/mamori/issues/55)) ([1333d05](https://github.com/xavidop/mamori/commit/1333d051b5344aa393feef4bdc637a991bf719c3))

# [1.3.0](https://github.com/xavidop/mamori/compare/v1.2.4...v1.3.0) (2026-07-27)


### Features

* security vulns ([#51](https://github.com/xavidop/mamori/issues/51)) ([03f1f90](https://github.com/xavidop/mamori/commit/03f1f90c64440a6760d47c77b31cdf510190437e))

## [1.2.4](https://github.com/xavidop/mamori/compare/v1.2.3...v1.2.4) (2026-07-26)


### Bug Fixes

* fleaky test ([60e0fbe](https://github.com/xavidop/mamori/commit/60e0fbe4d071c199da3044bfb417a4c35cbf603a))

## [1.2.3](https://github.com/xavidop/mamori/compare/v1.2.2...v1.2.3) (2026-07-26)


### Bug Fixes

* release ([60bec81](https://github.com/xavidop/mamori/commit/60bec81f191c01d420ca4d9a6039836cf0d11978))

## [1.2.2](https://github.com/xavidop/mamori/compare/v1.2.1...v1.2.2) (2026-07-26)


### Bug Fixes

* release ([4ec5bd8](https://github.com/xavidop/mamori/commit/4ec5bd88e05d634e3bbb7edb254bd3341519806e))

## [1.2.1](https://github.com/xavidop/mamori/compare/v1.2.0...v1.2.1) (2026-07-26)


### Bug Fixes

* release ([e5bc2d3](https://github.com/xavidop/mamori/commit/e5bc2d3c83155fe33dfb39d425b34c14c73c5a66))

# [1.2.0](https://github.com/xavidop/mamori/compare/v1.1.8...v1.2.0) (2026-07-26)


### Features

* mamori more feautres ([#47](https://github.com/xavidop/mamori/issues/47)) ([f84639b](https://github.com/xavidop/mamori/commit/f84639ba5c8043e9d1188a703ec988b80bd7dd69))

## [1.1.8](https://github.com/xavidop/mamori/compare/v1.1.7...v1.1.8) (2026-07-26)


### Bug Fixes

* debounce watchFilePath to eliminate truncate-before-write race in TestFileProviderWatch ([#44](https://github.com/xavidop/mamori/issues/44)) ([c749f77](https://github.com/xavidop/mamori/commit/c749f7759e11d64a3c199b98a11bdb98abafdbaf))

## [1.1.7](https://github.com/xavidop/mamori/compare/v1.1.6...v1.1.7) (2026-07-20)


### Bug Fixes

* space ([932390f](https://github.com/xavidop/mamori/commit/932390f220b7ce91e7538c789a9ccf371b0b84c0))

## [1.1.6](https://github.com/xavidop/mamori/compare/v1.1.5...v1.1.6) (2026-07-20)


### Bug Fixes

* improved main page ([ca17769](https://github.com/xavidop/mamori/commit/ca1776916037cac43da4dc17ce28d459b4f3b980))

## [1.1.5](https://github.com/xavidop/mamori/compare/v1.1.4...v1.1.5) (2026-07-19)


### Bug Fixes

* web in mobile ([21e894b](https://github.com/xavidop/mamori/commit/21e894b61a49c4dc7d3ef14dd5a71ee12e81707d))

## [1.1.4](https://github.com/xavidop/mamori/compare/v1.1.3...v1.1.4) (2026-07-19)


### Bug Fixes

* docs ([caf677a](https://github.com/xavidop/mamori/commit/caf677af9df3fed5a161501a93632d3e8b34c244))

## [1.1.3](https://github.com/xavidop/mamori/compare/v1.1.2...v1.1.3) (2026-07-19)


### Bug Fixes

* go mod ([4c6e0a6](https://github.com/xavidop/mamori/commit/4c6e0a617ec151da322198f14206000990e72cec))

## [1.1.2](https://github.com/xavidop/mamori/compare/v1.1.1...v1.1.2) (2026-07-19)


### Bug Fixes

* release ([43baee9](https://github.com/xavidop/mamori/commit/43baee9407194da63c8d21897569a0f094dc14fb))

## [1.1.1](https://github.com/xavidop/mamori/compare/v1.1.0...v1.1.1) (2026-07-19)


### Bug Fixes

* lint ([45ee90f](https://github.com/xavidop/mamori/commit/45ee90fbbdbcb90a3b4b9dadab6baba929dc7f30))

# [1.1.0](https://github.com/xavidop/mamori/compare/v1.0.0...v1.1.0) (2026-07-19)


### Bug Fixes

* format and lint ([832a302](https://github.com/xavidop/mamori/commit/832a302f7618e073a25834848e8a9b63f8230b2a))


### Features

* release ([b459196](https://github.com/xavidop/mamori/commit/b4591967f7386ccad10c5af966102f9befde1f0c))

# 1.0.0 (2026-07-19)


### Features

* first release ([f379025](https://github.com/xavidop/mamori/commit/f379025d42dd59a7ce45529324a04373e923bba6))
