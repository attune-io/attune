# Changelog

All notable changes to this project will be documented in this file.

The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.17](https://github.com/attune-io/attune/compare/v0.1.16...v0.1.17) (2026-06-29)


### Bug Fixes

* auto-approve PRs from attune-release-bot ([#345](https://github.com/attune-io/attune/issues/345)) ([d4fb798](https://github.com/attune-io/attune/commit/d4fb7981fd0e64a05b2b12abd5a4cf98fcd9c3bd))
* dco merge skip, dependabot docs, token perms tightening + rebase helper for best scorecard ([#350](https://github.com/attune-io/attune/issues/350)) ([e301fbd](https://github.com/attune-io/attune/commit/e301fbd62a606797cafa434a1eb1971fbee9c0e6))
* exclude dependabot from auto-approve and allow all semver types in auto-merge ([#355](https://github.com/attune-io/attune/issues/355)) ([e2a667b](https://github.com/attune-io/attune/commit/e2a667bb64a2b1e780dc474e1bf05f94e73cf67a))
* rebase-dependabot.sh missing origin/ prefix and update Dependabot docs ([#356](https://github.com/attune-io/attune/issues/356)) ([c5d8c67](https://github.com/attune-io/attune/commit/c5d8c67a19ab202b8cb52ace3f0930e6892f15b4))

## [0.1.16](https://github.com/attune-io/attune/compare/v0.1.15...v0.1.16) (2026-06-21)


### Features

* support optional RELEASE_NOTES.md override for curated release notes ([#339](https://github.com/attune-io/attune/issues/339)) ([dca0a52](https://github.com/attune-io/attune/commit/dca0a524f40fc83d007734a70805fa267d9dcf7f))


### Bug Fixes

* add retry and caching to cert-manager manifest download in e2e-nightly ([#324](https://github.com/attune-io/attune/issues/324)) ([3d232b7](https://github.com/attune-io/attune/commit/3d232b7be39da3324b02315da3ffa1d70ad6897f)), closes [#322](https://github.com/attune-io/attune/issues/322)
* e2e transient download failures with cached Chainsaw and helm retry ([#321](https://github.com/attune-io/attune/issues/321)) ([49855a3](https://github.com/attune-io/attune/commit/49855a31a921ec4f9c2e39aef13a440823f23c57)), closes [#317](https://github.com/attune-io/attune/issues/317)
* filter FOSSA false positive for pinned k8s.io/client-go ([#334](https://github.com/attune-io/attune/issues/334)) ([11ae86b](https://github.com/attune-io/attune/commit/11ae86b550b1bd4ba4dc94a01c41e9526d8b5d76))
* remove confidence floor and add QoS-aware HPA target cap ([#335](https://github.com/attune-io/attune/issues/335)) ([f92e9d4](https://github.com/attune-io/attune/commit/f92e9d48fe26b530ba347f6989b62cd7bebe6c93))
* unnecessary %% escapes in test messages and invalid jq parent filter ([#314](https://github.com/attune-io/attune/issues/314)) ([82e4670](https://github.com/attune-io/attune/commit/82e4670a6d87b8f3e5d39f66a1ee899c4c897223))
* use feature-gates= (set) instead of += (append) in k3s v1.32 config ([#326](https://github.com/attune-io/attune/issues/326)) ([d8bb805](https://github.com/attune-io/attune/commit/d8bb805b12ef43dd428bbc24de9af7d41121f2a2)), closes [#325](https://github.com/attune-io/attune/issues/325)
* use post-resize CPU limit for HPA QoS-aware cap ([#336](https://github.com/attune-io/attune/issues/336)) ([779ab52](https://github.com/attune-io/attune/commit/779ab52175e2e019fefe1b2780deb571107c6454))

## [0.1.15](https://github.com/attune-io/attune/compare/v0.1.14...v0.1.15) (2026-06-07)


### Bug Fixes

* dashboard metric names, costPricing field names, PromQL escaping, and stale recommendations alert ([#301](https://github.com/attune-io/attune/issues/301)) ([cfd41f1](https://github.com/attune-io/attune/commit/cfd41f119bd7e8e761fd540b2c1793b61afdad74))
* strengthen memory test assertion and cache cert-manager manifest in E2E ([#310](https://github.com/attune-io/attune/issues/310)) ([e379079](https://github.com/attune-io/attune/commit/e379079e5d8bf5167d34c00462625f789f3c9bec))
* use feature-gates+= (append) in k3s config file, not = (replace) ([#300](https://github.com/attune-io/attune/issues/300)) ([bb3922e](https://github.com/attune-io/attune/commit/bb3922e055fe375fcd23e7dcaedac3ed291c06c5)), closes [#299](https://github.com/attune-io/attune/issues/299)
* use k3s config file for v1.32 feature gate instead of CLI args ([#297](https://github.com/attune-io/attune/issues/297)) ([0437ce4](https://github.com/attune-io/attune/commit/0437ce4a48a1aa8ac48bce96195736cafd0523da))

## [0.1.14](https://github.com/attune-io/attune/compare/v0.1.13...v0.1.14) (2026-06-03)


### Features

* implement OpenShift feature annotations support ([#269](https://github.com/attune-io/attune/issues/269)) ([9cfa42b](https://github.com/attune-io/attune/commit/9cfa42b6390e8e02fa7b46a5f02df45cff45065e)), closes [#264](https://github.com/attune-io/attune/issues/264)
* make OpenShift RBAC conditional via openshift.enabled Helm value ([#272](https://github.com/attune-io/attune/issues/272)) ([5fb9804](https://github.com/attune-io/attune/commit/5fb98044da6c0e4060599032c925900ff8657a1b))
* migrate cosign signing from deprecated flags to --bundle format ([#248](https://github.com/attune-io/attune/issues/248)) ([92aa773](https://github.com/attune-io/attune/commit/92aa773d52ec7845cad4d6940992dd516193dd68))


### Bug Fixes

* add fallback cosign signing for re-releases of older tags ([#246](https://github.com/attune-io/attune/issues/246)) ([12a01c1](https://github.com/attune-io/attune/commit/12a01c134f52a0fc6423f8f9478f22e57817629c))
* add missing RBAC for OpenShift TLS profile detection ([#270](https://github.com/attune-io/attune/issues/270)) ([e3fe1f9](https://github.com/attune-io/attune/commit/e3fe1f9da72f5f601cab3dcb46904abe9c054e50))
* cosign fallback signing uses --bundle for newer cosign versions ([#247](https://github.com/attune-io/attune/issues/247)) ([4aa1f93](https://github.com/attune-io/attune/commit/4aa1f9389fecd87b7a637cccbb360c049e75d988)), closes [#241](https://github.com/attune-io/attune/issues/241)
* cycle 19 improvements (RBAC fix, test coverage, doc consistency) ([#250](https://github.com/attune-io/attune/issues/250)) ([44a7014](https://github.com/attune-io/attune/commit/44a7014e5fa8bf709f306d18fc8de0a9a9b6340f))
* dependabot auto-merge signature verification and rebase method ([#267](https://github.com/attune-io/attune/issues/267)) ([6f4abce](https://github.com/attune-io/attune/commit/6f4abceebd63514c6931f79d5bf93c10b3a626ee))
* docs HPA event reason + UpdateStrategy value-to-pointer type ([#255](https://github.com/attune-io/attune/issues/255)) ([050a1d5](https://github.com/attune-io/attune/commit/050a1d5c400e65e1f7a4b778a54eaf2e5e1ebcae))
* handle partial API discovery and fix OpenShift doc log level ([#273](https://github.com/attune-io/attune/issues/273)) ([b549724](https://github.com/attune-io/attune/commit/b549724b0bef6128ef63ec8aac0088c7aaba19cd))
* modernize CRD short names (rsp-&gt;ap, rsd-&gt;ad, rsnd-&gt;and) ([#251](https://github.com/attune-io/attune/issues/251)) ([1783f79](https://github.com/attune-io/attune/commit/1783f79c795e3cffbdfb3fca58c345f50eeda056)), closes [#249](https://github.com/attune-io/attune/issues/249)
* parse Custom TLS profile minTLSVersion on OpenShift clusters ([#276](https://github.com/attune-io/attune/issues/276)) ([5482c4d](https://github.com/attune-io/attune/commit/5482c4dad88a41a0851b0d35ae7479574e4d63f7))
* pin OLM bundle images by digest and add relatedImages ([#263](https://github.com/attune-io/attune/issues/263)) ([5b5ea0f](https://github.com/attune-io/attune/commit/5b5ea0fb987312a29046c039636f646b8324d23e))
* pre-pull and cache k3s node image to prevent E2E cluster creation failures ([#287](https://github.com/attune-io/attune/issues/287)) ([3805728](https://github.com/attune-io/attune/commit/3805728febcb0940aba8677ddcfe0705ece54186))
* re-release workflow skips :latest tags and downstream jobs ([#242](https://github.com/attune-io/attune/issues/242)) ([2b6b5ac](https://github.com/attune-io/attune/commit/2b6b5ac2dc33ed120d748cd8d2c3a234b69bebef)), closes [#241](https://github.com/attune-io/attune/issues/241)
* remove auto-rebase job that silently broke Dependabot CI ([#268](https://github.com/attune-io/attune/issues/268)) ([9896a12](https://github.com/attune-io/attune/commit/9896a12f9531edd4d163aef1cb2995bfb590dcc7))
* rename OLM bundle CSV from attune-operator to attune ([#252](https://github.com/attune-io/attune/issues/252)) ([8bd71e7](https://github.com/attune-io/attune/commit/8bd71e72bccfe4a46bee008eaee0731549d6d0e3))
* resolve tech-debt issues [#278](https://github.com/attune-io/attune/issues/278)-[#281](https://github.com/attune-io/attune/issues/281) ([#283](https://github.com/attune-io/attune/issues/283)) ([0b2ad96](https://github.com/attune-io/attune/commit/0b2ad963f15f6e32d91f6544cf7df3546b66afbc))
* revert failure metric, NaN/Inf guard, and validator mutation ([#277](https://github.com/attune-io/attune/issues/277)) ([e12c537](https://github.com/attune-io/attune/commit/e12c537873297a20972d6f7ea5e1f8e643ca3b83))
* scorecard token-permissions and AI code quality findings ([#289](https://github.com/attune-io/attune/issues/289)) ([18d9497](https://github.com/attune-io/attune/commit/18d9497e01e0148ceee69a0fbbaa73b1db94c503))
* skip Docker build and downstream steps on re-releases ([#244](https://github.com/attune-io/attune/issues/244)) ([c92c9ff](https://github.com/attune-io/attune/commit/c92c9ff37196fc55b82594cb8bfa2248e028ce6e)), closes [#241](https://github.com/attune-io/attune/issues/241)
* skip Docker build on re-release to preserve OLM bundle digest ([#275](https://github.com/attune-io/attune/issues/275)) ([4ed8302](https://github.com/attune-io/attune/commit/4ed83026539863bc9d3ab49727810cbcc108ce16))
* skip Docker build on re-release when Dockerfile.release is missing ([#245](https://github.com/attune-io/attune/issues/245)) ([4f56811](https://github.com/attune-io/attune/commit/4f56811b9a4ff76cfd69ed1fb928373376492cfa)), closes [#241](https://github.com/attune-io/attune/issues/241)
* update Go 1.26.3 to 1.26.4 for stdlib CVE fixes ([#284](https://github.com/attune-io/attune/issues/284)) ([1a098cb](https://github.com/attune-io/attune/commit/1a098cb19ff71dba57763cc61434b64b3ca94bd9))

## [0.1.13](https://github.com/attune-io/attune/compare/v0.1.12...v0.1.13) (2026-06-01)


### Bug Fixes

* add --use-signing-config=false for cosign old bundle format ([#232](https://github.com/attune-io/attune/issues/232)) ([88c7a7f](https://github.com/attune-io/attune/commit/88c7a7fe7f47ccef6a81d07a36bfc73b2ee9e938))
* add cosign signing to GoReleaser and workflow for retroactive release signing ([#228](https://github.com/attune-io/attune/issues/228)) ([365c124](https://github.com/attune-io/attune/commit/365c1240939ada7ab10d5572702591ac70315ddb))
* address 4 GitHub AI code quality findings ([#235](https://github.com/attune-io/attune/issues/235)) ([0c2fbd3](https://github.com/attune-io/attune/commit/0c2fbd3f1ee81db617075bd8013cf9f9443469a1))
* cosign signing with --new-bundle-format=false for scorecard ([#231](https://github.com/attune-io/attune/issues/231)) ([ac9b124](https://github.com/attune-io/attune/commit/ac9b1241b080683095ac02a27945e074d2a61139))
* docs, CI consistency, and demo script fixes from multi-perspective review ([#226](https://github.com/attune-io/attune/issues/226)) ([d300a11](https://github.com/attune-io/attune/commit/d300a114a775114986ce1133d69e0bf538673f17))
* exclude helm.sh from lychee link checks ([#236](https://github.com/attune-io/attune/issues/236)) ([6fe7766](https://github.com/attune-io/attune/commit/6fe77668374bbc42e4f5d37b2930f35c91578fd5))
* pin setup-oras to SHA that includes ORAS CLI 1.3.2 ([#224](https://github.com/attune-io/attune/issues/224)) ([a206aef](https://github.com/attune-io/attune/commit/a206aef889a16d9079f4c3810b7f55ef72e92e94)), closes [#221](https://github.com/attune-io/attune/issues/221)
* switch auto-approve to GITHUB_TOKEN pattern and fix retroactive signing ([#229](https://github.com/attune-io/attune/issues/229)) ([2a096d4](https://github.com/attune-io/attune/commit/2a096d4b8f2469afa7620de85ad31e61c48b6fc9))
* use cosign --bundle flag for SBOM signing ([#213](https://github.com/attune-io/attune/issues/213)) ([afc6026](https://github.com/attune-io/attune/commit/afc602623ba631534f73f9697144ae7e214e3c5b))
* use oras cp for Docker Hub Helm chart push ([#220](https://github.com/attune-io/attune/issues/220)) ([6ecbaee](https://github.com/attune-io/attune/commit/6ecbaeed0221445cc4f4a484f3dd21423769924b)), closes [#218](https://github.com/attune-io/attune/issues/218)

## [0.1.12](https://github.com/attune-io/attune/compare/v0.1.11...v0.1.12) (2026-05-31)


### Bug Fixes

* docker Hub chart separation + nightly E2E install-binary-tool PATH fix ([#208](https://github.com/attune-io/attune/issues/208)) ([f8db24e](https://github.com/attune-io/attune/commit/f8db24e53da35a66aee84e895c0c41bd81c90b3d))
* release pipeline audit fixes ([#207](https://github.com/attune-io/attune/issues/207)) ([4bdb78f](https://github.com/attune-io/attune/commit/4bdb78fab2839ab8f871c2eecc1d4a684fc5b6ff)), closes [#198](https://github.com/attune-io/attune/issues/198) [#199](https://github.com/attune-io/attune/issues/199) [#200](https://github.com/attune-io/attune/issues/200) [#201](https://github.com/attune-io/attune/issues/201) [#202](https://github.com/attune-io/attune/issues/202) [#203](https://github.com/attune-io/attune/issues/203) [#204](https://github.com/attune-io/attune/issues/204) [#205](https://github.com/attune-io/attune/issues/205) [#206](https://github.com/attune-io/attune/issues/206)
* use PAT for OperatorHub upstream PR creation ([#196](https://github.com/attune-io/attune/issues/196)) ([f469ec2](https://github.com/attune-io/attune/commit/f469ec269618d432cbf3514500db44474381a93e)), closes [#195](https://github.com/attune-io/attune/issues/195)

## [0.1.11](https://github.com/attune-io/attune/compare/v0.1.10...v0.1.11) (2026-05-31)


### Bug Fixes

* replace deprecated archives.builds with archives.ids ([#189](https://github.com/attune-io/attune/issues/189)) ([21c3ef3](https://github.com/attune-io/attune/commit/21c3ef346f4eb0437832b8ab0ca9dee1cbab4609))
* use explicit checkout path in operatorhub-pr.sh instead of OLDPWD ([#190](https://github.com/attune-io/attune/issues/190)) ([a118898](https://github.com/attune-io/attune/commit/a11889812f96de0d1e34bfdd5c49c7ebdd815685))

## [0.1.10](https://github.com/attune-io/attune/compare/v0.1.9...v0.1.10) (2026-05-31)


### Features

* add full export mode awareness and `kubectl attune export list` to CLI ([#147](https://github.com/attune-io/attune/issues/147)) ([673217a](https://github.com/attune-io/attune/commit/673217a68b56d8391ab795143358f03564215364))
* add Prometheus metrics for request clamping and NaN/Inf data quality ([#177](https://github.com/attune-io/attune/issues/177)) ([8ab69a3](https://github.com/attune-io/attune/commit/8ab69a380a0a1f39280eaf0c85541844cc1cf8a7)), closes [#174](https://github.com/attune-io/attune/issues/174)
* show all effective fields in kubectl attune explain ([#158](https://github.com/attune-io/attune/issues/158)) ([6df1d9b](https://github.com/attune-io/attune/commit/6df1d9bfadca9dd69357bb41b3f3726ff9989325))


### Bug Fixes

* add observability logging for request clamping and NaN/Inf data quality ([#172](https://github.com/attune-io/attune/issues/172)) ([35d3e65](https://github.com/attune-io/attune/commit/35d3e650b19efc957ad97b952502783fc9341e61))
* **defaults:** merge SLOGuardrails from AttuneDefaults into policies ([e5f106f](https://github.com/attune-io/attune/commit/e5f106f18aa1c365cd42ea4dde940e757f349a98))
* eliminate eventDedup race condition and unbounded map growth ([a2b1f08](https://github.com/attune-io/attune/commit/a2b1f08aec17673591e361f776b9e418d90e8a29))
* guard SLO guardrail query values against NaN and Inf ([#167](https://github.com/attune-io/attune/issues/167)) ([2909801](https://github.com/attune-io/attune/commit/29098016cb4ca0d31cf7449aa3117ca86261ea1c))
* **helm:** set category to monitoring-logging, remove prerelease flag ([a10d5fd](https://github.com/attune-io/attune/commit/a10d5fd50b35bdf442e84911060c02f920ef6f24))
* **helm:** use computed replica count for PDB rendering ([d7fd9ee](https://github.com/attune-io/attune/commit/d7fd9ee2b5408567e9d49085c6a329826266f111))
* orphan cleanup for recommendation ConfigMaps when workloads leave policy scope ([#140](https://github.com/attune-io/attune/issues/140)) ([b280079](https://github.com/attune-io/attune/commit/b28007990f6b9f11106a7716ca76c56894ac2a8c))
* remove untrusted code checkout from pr-size workflow ([#187](https://github.com/attune-io/attune/issues/187)) ([cf5785e](https://github.com/attune-io/attune/commit/cf5785e6aea5df9c980ed72eec13c73bb6e8ed00))
* replace inline Python with jq in orphan-cleanup E2E test ([#153](https://github.com/attune-io/attune/issues/153)) ([d333fe9](https://github.com/attune-io/attune/commit/d333fe9eff8258a66a47912f34f9669f968d34fc)), closes [#150](https://github.com/attune-io/attune/issues/150)
* replace last direct AttunePolicyReconciler struct literal with constructor ([#160](https://github.com/attune-io/attune/issues/160)) ([ecb6b47](https://github.com/attune-io/attune/commit/ecb6b4710985baf6a20f96cb1ff87b72b3c20963)), closes [#141](https://github.com/attune-io/attune/issues/141)
* resolve lint and YAML formatting issues ([#155](https://github.com/attune-io/attune/issues/155)) ([15606f2](https://github.com/attune-io/attune/commit/15606f2781a20b0e29110e18decc04da83bf6162))
* review findings from 24h audit ([#154](https://github.com/attune-io/attune/issues/154)) ([a486428](https://github.com/attune-io/attune/commit/a48642870646e457a33f02ab06106b7047758156))
* **safety:** propagate revert reason to resize history entries ([afdcdfa](https://github.com/attune-io/attune/commit/afdcdfa02279cf513f3c89bc6cca24e36a1b5e58))
* test coverage gaps, data quality alerts, dashboard panels, and SPEC.md metrics ([#184](https://github.com/attune-io/attune/issues/184)) ([78faa77](https://github.com/attune-io/attune/commit/78faa77edf198f41d9c24f1b93e41d3dde6f39da)), closes [#179](https://github.com/attune-io/attune/issues/179) [#180](https://github.com/attune-io/attune/issues/180) [#181](https://github.com/attune-io/attune/issues/181) [#183](https://github.com/attune-io/attune/issues/183)
* update stale DCO reference, add missing CRD metadata, wire verify script ([9a87766](https://github.com/attune-io/attune/commit/9a87766f4b4302355fe883221224f55e48db3647))
* use spec.targetRef.selector in orphan-cleanup E2E test ([#156](https://github.com/attune-io/attune/issues/156)) ([e3d8971](https://github.com/attune-io/attune/commit/e3d89717da574e586669ebceccbd1ceb321145b3))
* **webhook:** validate memoryFromCpuRatio value at admission ([57e754f](https://github.com/attune-io/attune/commit/57e754f5d9f36abeb9ed272370da33c051d3a851))

## [0.1.9](https://github.com/attune-io/attune/compare/v0.1.8...v0.1.9) (2026-05-29)


### Bug Fixes

* **krew:** remove s390x platform (rejected by krew-index validator) ([67a7b8f](https://github.com/attune-io/attune/commit/67a7b8f971425a5ee79520624a60e864e746e7ec))
* **krew:** remove subcommand list from description per maintainer review ([5d96ff3](https://github.com/attune-io/attune/commit/5d96ff37640d25fa060f323701f3457fb49e0c3a))

## [0.1.8](https://github.com/attune-io/attune/compare/v0.1.7...v0.1.8) (2026-05-29)


### Features

* **ci:** automate OperatorHub bundle submission in release workflow ([4aacfa7](https://github.com/attune-io/attune/commit/4aacfa700a6eb94971973091221fe04301d8dd9d)), closes [#131](https://github.com/attune-io/attune/issues/131)
* **helm:** add AttuneBudgetExhausted PrometheusRule alert ([6bc68e3](https://github.com/attune-io/attune/commit/6bc68e3fd29eb4ee06d9c2592caac3ec45698886))
* **krew:** add ppc64le and s390x platform support ([89ae79d](https://github.com/attune-io/attune/commit/89ae79d8a0cb2007b468bed349d5082db07537ca))
* replace personal PAT with GitHub App for OperatorHub PRs ([4804109](https://github.com/attune-io/attune/commit/480410907c8094cdd50234538879d42ae8a22104)), closes [#135](https://github.com/attune-io/attune/issues/135)


### Bug Fixes

* add NaN/Inf guards to remaining ParseFloat call sites ([315801c](https://github.com/attune-io/attune/commit/315801cbe040bf43f5df635faf1e85a557277e42))
* add reviewers to OperatorHub ci.yaml for auto-merge ([a5349c2](https://github.com/attune-io/attune/commit/a5349c2e684ae13a1b8b2998c89fb980525678a2))
* **ci:** disable errexit around E2E gate API polling ([894ca93](https://github.com/attune-io/attune/commit/894ca939b2d0683a5930e302ab6cf88ce1bb0909))
* **ci:** handle transient API failures in E2E lint/unit gate polling ([d70ed2e](https://github.com/attune-io/attune/commit/d70ed2e4961e5b40fbdf2a9f32e993998718e172))
* **ci:** make release workflow idempotent for re-runs ([b698035](https://github.com/attune-io/attune/commit/b698035ee8d0047d178192cc980e7204d356a201))
* **ci:** pass explicit tag_name to softprops/action-gh-release ([fcf0d95](https://github.com/attune-io/attune/commit/fcf0d95c7fdf451bd39b7618c75a26df7ffc2223))
* **ci:** replace E2E gate polling with needs dependency ([b6aeebf](https://github.com/attune-io/attune/commit/b6aeebfa8d22cc73efaf506d875496619a2b3062))
* **ci:** separate API call from jq to prevent gate false-failures ([2a32234](https://github.com/attune-io/attune/commit/2a3223417e48a82251bddf0585f91e0d64b60596))
* **ci:** use GitHub App token for release-please ([247d0d0](https://github.com/attune-io/attune/commit/247d0d01a8a54d478655d02b8663058cf3c5052b))
* **ci:** use SVG logo in OLM bundle and generate icon at build time ([#132](https://github.com/attune-io/attune/issues/132)) ([e47df9a](https://github.com/attune-io/attune/commit/e47df9ae1b171372f2454795abd5c944378d5621))
* **helm:** set operator capability level to Auto Pilot ([825a77f](https://github.com/attune-io/attune/commit/825a77f913de4bfd03763f42ac827b7154c40635))
* **plugin:** show burst factor in explain output and improve CRD-missing error ([88e753c](https://github.com/attune-io/attune/commit/88e753c1373b02af89bb00a2fd96e56ac645b344))
* **webhook:** reject NaN and Inf in SLO guardrail threshold ([e978e35](https://github.com/attune-io/attune/commit/e978e35a25caa5c6b49c17846729c5aee265019a))

## [0.1.7](https://github.com/attune-io/attune/compare/v0.1.6...v0.1.7) (2026-05-29)


### Features

* add FIPS 140-3 compliance toggle ([f0406c5](https://github.com/attune-io/attune/commit/f0406c573b2b95ff417e943acd6468b119ca5378))


### Bug Fixes

* krew template indentation for addURIAndSha output ([0e3fa5f](https://github.com/attune-io/attune/commit/0e3fa5f9f007421f4de63d9395de2e2d982a5629))

## [0.1.6](https://github.com/attune-io/attune/compare/v0.1.5...v0.1.6) (2026-05-28)


### Bug Fixes

* krew-release-bot template uses unsupported PluginOwner/PluginRepo vars ([3a26c65](https://github.com/attune-io/attune/commit/3a26c6593931d6f708d005203a4a71d478d531b5))
* **release:** add Docker Hub login for Helm chart cosign signing ([76fdfce](https://github.com/attune-io/attune/commit/76fdfce16ba418c953e76789d6900569446e7b6f)), closes [#128](https://github.com/attune-io/attune/issues/128)
* remove unsupported ppc64le/s390x from krew manifest ([28de433](https://github.com/attune-io/attune/commit/28de4339aee61b752437e628231840fed1e6ebf1))
* SVG logo arc proportions, needle shape, and pivot position ([bee00fc](https://github.com/attune-io/attune/commit/bee00fc4f60e2f46d652a33dd4531038b766b150)), closes [#126](https://github.com/attune-io/attune/issues/126)
* SVG logo needle and pivot to match PNG reference ([bf47fca](https://github.com/attune-io/attune/commit/bf47fca848d907ed5107bd43b4a523c008151ce7)), closes [#126](https://github.com/attune-io/attune/issues/126)

## [0.1.5](https://github.com/attune-io/attune/compare/v0.1.4...v0.1.5) (2026-05-28)


### Features

* add Artifact Hub listing with verified publisher metadata ([9f72677](https://github.com/attune-io/attune/commit/9f72677ef3e1db0c478e4ac7dee8cdcee0fb90d1)), closes [#106](https://github.com/attune-io/attune/issues/106)
* enrich Helm chart metadata for Artifact Hub listing ([cf9bcc7](https://github.com/attune-io/attune/commit/cf9bcc72a038a4b3715eb4111f7a564098a52fa7))


### Bug Fixes

* add logo.jpg for Artifact Hub compatibility ([3c7b13b](https://github.com/attune-io/attune/commit/3c7b13b96abee314a4ace36fbf4a8164a433b8d3))
* remove logo.jpg, keep only PNG ([c4560b2](https://github.com/attune-io/attune/commit/c4560b2d558645a8533a348bd2818476af5e8d20))

## [0.1.4](https://github.com/attune-io/attune/compare/v0.1.3...v0.1.4) (2026-05-27)


### Features

* add arm/v7, ppc64le, and s390x architecture support ([bc3f814](https://github.com/attune-io/attune/commit/bc3f814283533990e4377c94c0f43750bd554aac))


### Bug Fixes

* use stable checksums filename for SLSA provenance ([6e5c151](https://github.com/attune-io/attune/commit/6e5c151989e8f6c99118dac5a29ef72126fbdd3d))

## [0.1.3](https://github.com/attune-io/attune/compare/v0.1.2...v0.1.3) (2026-05-27)


### Bug Fixes

* use tag refs for SLSA provenance reusable workflows ([d1ac09a](https://github.com/attune-io/attune/commit/d1ac09a6f604cfc51318d0d73a14d4947b657bc4))

## [0.1.2](https://github.com/attune-io/attune/compare/v0.1.1...v0.1.2) (2026-05-27)


### Features

* publish container image to Docker Hub for discoverability ([#102](https://github.com/attune-io/attune/issues/102)) ([9f8ffd5](https://github.com/attune-io/attune/commit/9f8ffd5fd1a1baa5e6d59b49ea46505c0bad94dd))

## [0.1.1](https://github.com/attune-io/attune/compare/v0.1.0...v0.1.1) (2026-05-27)


### Bug Fixes

* **ci:** pin all transitive pip dependencies with hashes ([#85](https://github.com/attune-io/attune/issues/85)) ([9cafc42](https://github.com/attune-io/attune/commit/9cafc4221dd6b3fa14ea1a15479e70f14a9d0611))
* convert logo from JPG to PNG with transparent corners ([#43](https://github.com/attune-io/attune/issues/43)) ([fcb3b23](https://github.com/attune-io/attune/commit/fcb3b23355ee934f7e6e9b9cee89cef89c5ca209))
* correct hallucinated email in artifacthub-repo.yml ([#56](https://github.com/attune-io/attune/issues/56)) ([4a50290](https://github.com/attune-io/attune/commit/4a50290e156723339fea0a7cf91e591faebc5aea))
* e2e nightly RealisticLoad timeout + safe cache keys for secrets (no SHA256) ([#44](https://github.com/attune-io/attune/issues/44)) ([2bed71a](https://github.com/attune-io/attune/commit/2bed71a0fcb58a241b17385d463ffd61070f183a))
* **e2e:** replace stress-ng with busybox CPU burn and update SECURITY.md ([#86](https://github.com/attune-io/attune/issues/86)) ([de76adc](https://github.com/attune-io/attune/commit/de76adc97cb994096f4ca6779b48b7dfd8c5da7f))
* **e2e:** resolve recommend-mode Chainsaw intermittent timeout ([#92](https://github.com/attune-io/attune/issues/92)) ([c50e2ac](https://github.com/attune-io/attune/commit/c50e2acdf288d2040d86d2bb653311db6c4d53a8))
* **e2e:** use explicit Command for stress-ng and add deployment diagnostics ([#83](https://github.com/attune-io/attune/issues/83)) ([c269cdf](https://github.com/attune-io/attune/commit/c269cdf908adc125bb0190bce5f2192bf666cb24))
* remove stress-ng --vm stressor from RealisticLoad E2E test ([#59](https://github.com/attune-io/attune/issues/59)) ([6b7efa9](https://github.com/attune-io/attune/commit/6b7efa9a39682680c4d8ff5d7d5cc17f55d35aca))
* scope workflow token permissions to job level for Scorecard ([#42](https://github.com/attune-io/attune/issues/42)) ([5251468](https://github.com/attune-io/attune/commit/52514682e36a307e1a0c4a235d0a76255888f79a))
* stabilize Chainsaw tests and add govulncheck to CI gate ([#95](https://github.com/attune-io/attune/issues/95)) ([011c8ad](https://github.com/attune-io/attune/commit/011c8adc56ee9c6a0e43cf2c13dfec27f18862f4))

## [Unreleased]

## [0.1.0] - 2025-05-26

### Added

- Support for Kubernetes 1.32 with `InPlacePodVerticalScaling` alpha feature gate; the operator now falls back to the deprecated `pod.Status.Resize` field for resize status on clusters without the 1.33+ pod conditions
- Top-level `safetyObservationPeriod` field on `UpdateStrategy` for configuring post-resize safety watch duration (default 5m, minimum 1m); takes precedence over `canary.observationPeriod` and works in all modes
- Early OOMKill and crash loop detection during safety observation period: critical events trigger immediate revert without waiting for the full observation period
- `kubectl attune explain` now displays the effective observation period with source tracking
- Configurable `rateWindow` field for CPU PromQL queries; no longer hardcoded to `[5m]`, now tracks `queryStep` by default
- Effective cooldown with backoff multiplier exposed in policy status
- Recommendation staleness detection with `LastDataTime` and `Stale` fields; stale recommendations block resize execution
- `StaleRecommendationsTotal` metric for tracking Prometheus degradation
- `ScheduleBlocked` status condition when outside the configured resize window
- `SCHEDULE` column in `kubectl attune status` output
- Per-policy namespace/name labels on `ReconcileDuration` metric
- Per-policy reconcile duration panel in Grafana dashboard (p99/p50 by namespace and policy)
- ReplicaSet as a supported target workload kind with adapter, RBAC, and Helm clusterrole
- Cross-namespace Secret reference rejection in webhook validation
- `AttuneHighRevertRate` PrometheusRule alert in Helm chart
- Configurable `burstSensitivity` per resource: controls how much burst detection inflates recommendations (default 0.1, set 0 to disable)
- Canary auto-promotion resets on spec change: editing a policy restarts the observation cycle so new configuration is re-validated
- `attune_burst_factor` Prometheus metric and Grafana dashboard panel showing burst detection multiplier per workload
- Burst detection now influences recommendations via logarithmic safety-margin boost
- Canary auto-promotion: when `autoPromote: true`, the operator automatically promotes to full fleet resize after the observation period passes without safety violations
- VPA conflict detection E2E test (Chainsaw scenario with inline CRD)
- OOMKill safety revert Go E2E test (uses stress-ng for reliable OOMKill trigger)
- Helm values schema validation (`values.schema.json`) for catching typos at install time
- Pending workloads column in `kubectl attune status` output
- Secret name and key context in Prometheus auth failure messages
- Go E2E tests for bearer-token Secret rotation and recommendations without live pods
- Structured-output test coverage for kubectl plugin (`-o json`, `-o yaml`)
- Documentation for running the full Go E2E suite locally
- V(1) debug log when a resize is skipped because the container is already at the target resources
- **Initial sizing webhook**: Mutating admission webhook sets pod resource requests/limits at creation time based on existing AttunePolicy recommendations, eliminating the "deploy with bad defaults" gap. Requires namespace label `attune.io/initial-sizing=enabled` and `initialSizing: true` on the policy. Safety: `failurePolicy: Ignore`, confidence threshold 0.5, stale check.
- **Directional change caps**: `maxIncreasePercent` (default 50%) and `maxDecreasePercent` (default 30%) in ResourceConfig for asymmetric per-step caps (memory decreases are riskier than CPU increases)
- **Memory-from-CPU derivation**: `memoryFromCpuRatio` in ResourceConfig derives memory recommendation from CPU (e.g., `"2.0"` for JVM heap-bound workloads), skipping Prometheus memory queries
- Wizard `create` and `promote` flows now prompt for initial sizing when mode is Auto, OneShot, or Canary
- **SLO-based guardrails**: `updateStrategy.sloGuardrails[]` defines application-level PromQL checks (latency, error rate) evaluated after each resize during the safety observation period. Breaching a threshold triggers automatic revert. Supports template variables for namespace, workload, and pod name.
- **VPA recommendation consumption**: `metricsSource.vpa` consumes existing VerticalPodAutoscaler recommendations as an alternative to Prometheus queries, bridging VPA-only clusters into Attune's in-place resize engine
- **GitOps diff command**: `kubectl attune diff` outputs resource change recommendations in YAML diff format for GitOps workflows (ArgoCD, Flux). Supports `-o yaml` structured output.
- **spec.paused**: Boolean field on `AttunePolicySpec` that halts all reconciliation (metrics collection, recommendations, resizes) without reverting existing resizes. The operator sets `Ready=False` with `reason=Paused`. Modeled after Prometheus Operator and Flux `spec.suspend`.
- **Webhook warnings for nonsensical config**: 13 admission-time warnings detect ineffective settings (e.g., canary config in non-canary mode, SLO guardrails with VPA source, resize-only settings in Observe/Recommend mode)
- **Runtime K8s events**: 31 warning/event types (up from 3) for silent controller behaviors: `StaleRecommendation`, `CooldownActive`, `HPAConflict`, `VPAConflict`, `ConfigClamped`, `ExportFailed`, `ResizeSkipped`, `BudgetExhausted`, and more. All recurring events use 1-hour deduplication to prevent log spam.
- **Warning suppression**: `attune.io/suppress-warnings` annotation accepts a comma-separated list of event reasons to suppress (e.g., `HPAConflict,ConfigClamped`)

### Changed

- **BREAKING**: `safetyMargin` field renamed to `overhead` with percentage semantics. Old multiplier values must be converted: `(old - 1) * 100` (e.g., `safetyMargin: "1.2"` becomes `overhead: "20"`). Defaults changed from `"1.2"`/`"1.3"` to `"20"`/`"30"`. Validation bounds changed from `(0, 10.0]` to `[0, 900]`.
- **BREAKING**: `maxCpuChangePercent` and `maxMemoryChangePercent` moved from `updateStrategy` to `cpu`/`memory` as `maxChangePercent`. Groups all per-resource recommendation parameters in one place.
- **BREAKING**: `updateStrategy.mode` field renamed to `updateStrategy.type` to align with Kubernetes core conventions
- **BREAKING**: `bounds.min`/`bounds.max` renamed to `minAllowed`/`maxAllowed`, `InPlaceOrEvict` renamed to `InPlaceOrRecreate`, `excludeContainers` renamed to `excludedContainers`
- Shorter requeue interval during data collection phase for faster initial recommendation generation
- `canary.percentage` CRD minimum changed from 0 to 1 (a 0% canary is meaningless)
- `rateWindow` is inheritable via `AttuneDefaults` and `AttuneNamespaceDefaults`
- Deployment-owned ReplicaSets are filtered from target discovery to prevent double-resizing
- Reconcile predicate filters out self-triggered status and metadata updates, reducing kube-apiserver load by eliminating 2-3x reconcile amplification per cycle
- Recommendations no longer require live pods; historical Prometheus data is sufficient for recommend-only flows
- Secret-backed bearer tokens are refreshed on every reconcile instead of being cached until TTL expiry
- Collector cache identity uses hashed token values instead of plain presence markers
- Extracted `buildCollectorOptions` helper from the main `Reconcile` method
- Documentation now clarifies that `minimumDataPoints` counts Prometheus range-query samples, so wall-clock recommendation timing depends on `queryStep`
- Reserved Prometheus query parameters (`query`, `start`, `end`, `step`, `time`, `timeout`) are now rejected so operator-managed request keys cannot be overridden

### Fixed

- `golang.org/x/net` updated to v0.55.0 to fix GO-2026-5026 (Punycode validation vulnerability in `idna`)
- Trivy image scan CI failure on runners without BuildKit/buildx; the step now strips BuildKit-only Dockerfile directives and builds natively with the legacy builder
- `make docker-build` now sets `DOCKER_BUILDKIT=1` so the Dockerfile's `--platform=$BUILDPLATFORM` resolves on legacy Docker CLIs
- `kubectl attune explain` was missing `safetyObservationPeriod` merge from namespace/cluster defaults, showing wrong effective value
- `StaleRecommendationsTotal` metric label mismatch between registration and increment
- E2E test flakes: OOMKill timeout, GuaranteedQoS queryStep, ScaleUp timeout, Chainsaw poll intervals, rateWindow regression with short queryStep
- Status race condition where concurrent reconciles could reset `status.workloads.resized` to 0 after a successful resize; Resized count is now derived from resize history entries which survive optimistic concurrency conflicts
- `attune_throttle_deferred_total` metric now appears in the Grafana dashboard (was the only unvisualized operator metric)
- `AttuneNamespaceDefaults` CRD missing from `config/crd/kustomization.yaml`; kustomize deployments now include it
- Bearer-token cache prefix collision when one Prometheus address is a prefix of another
- `make test-local` now cleans up the k3d cluster even on mid-run failures
- Gitleaks PATH resolution on self-hosted runners
- `prometheus-unreachable` E2E test now accepts either `InsufficientData` or `PrometheusUnavailable` reason, fixing a flake where the first reconcile sets one reason and subsequent reconciles set another
- RevertPod now retries on 409 Conflict (matching ResizePod); previously a conflict during revert left the pod at unsafe resource levels until the next reconcile
- Datadog and CloudWatch collector caches now share the same TTL eviction, capacity bounds, and race-safe LoadOrStore as the Prometheus collector cache; previously they could leak memory and create duplicate collectors
- Startup boost expiry pre-check now includes memory values, preventing node allocatable safety check bypass when namespaces have memory LimitRange constraints
- Annotation cleanup in safety observation now retries on 409 Conflict (up to 3 attempts), matching the persistResizeAnnotations retry pattern
- Multi-container sequential resize: annotation persist now retries on 409 Conflict instead of reverting the second container
- Memory limit clamp for K8s v1.33: in-place memory limit decreases are skipped when the container's resize policy is `NotRequired`, preventing API server rejection
- Guaranteed QoS preservation with memory limit clamp: the clamp is applied before the QoS check so that Guaranteed pods are not incorrectly resized into Burstable
- `helm-unittest` download now uses dynamic OS/arch detection instead of hardcoded `linux-amd64`
- OOMKill E2E test: `RestartContainer` memory resize policy hides OOM evidence by overwriting `LastTerminationState` on resize-induced restarts; test now uses `NotRequired` policy
- Safety revert path now applies K8s v1.33 memory limit clamp (`ClampMemoryLimitForPolicy`), preventing revert failures when memory limits would decrease with `NotRequired` resize policy
- CI image builds switched from Docker/BuildKit to `ko`, eliminating Docker daemon dependency and containerd storage race conditions on macOS self-hosted runners
- k3d image import retry loops with pre-cleanup for macOS containerd storage flakes
- Confidence factor formula `(1+M/C)^E` produced a 4x multiplier at maximum confidence (7 days of data), inflating all recommendations well beyond the user's configured overhead. A workload with P95=200m and `overhead: "20"` converged to ~960m instead of the expected ~240m. Replaced with `1 + M*(1-C)^E` which gives factor=1.0 at full confidence and up to 1.8x at minimum confidence.
- `memoryFromCpuRatio` values above 10.0 (e.g., `"16.0"` for in-memory databases) were silently rejected by the shared `parseFloat64` parser, disabling the feature without any error or warning. The ratio now uses a dedicated parser with a 1000.0 ceiling.
