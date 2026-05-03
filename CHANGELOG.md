# Changelog

## [0.3.0](https://github.com/RuckStackApp/locationindex/compare/v0.2.0...v0.3.0) (2026-05-03)


### Features

* describe supported location index queries ([d7c8179](https://github.com/RuckStackApp/locationindex/commit/d7c8179febdcf0ce7383609dec0ed631ea8a0c5a))

## [0.2.0](https://github.com/RuckStackApp/locationindex/compare/v0.1.0...v0.2.0) (2026-05-03)


### Features

* add basic concurrency safety to location index ([93c3cad](https://github.com/RuckStackApp/locationindex/commit/93c3cad81bb9e61fc37585468c6ff0b8554dde8d))
* add index cli and benchmark-driven query optimizations ([cfaa91e](https://github.com/RuckStackApp/locationindex/commit/cfaa91ea4c1194557720793a9c4dbe0d880cee89))
* add index stats for observability ([74fd775](https://github.com/RuckStackApp/locationindex/commit/74fd775bc1a1dc95cb4a2351aa92df18206e4d0f))
* add snapshot and clone lifecycle APIs ([d62fe76](https://github.com/RuckStackApp/locationindex/commit/d62fe76db6b842138642eafc94c08daf16ae68f6))
* compress persisted postings with delta varints ([995ebdd](https://github.com/RuckStackApp/locationindex/commit/995ebdd5ba22809c1ab3640855be5ac040ce491e))
* document index write model and lifecycle usage ([30398ee](https://github.com/RuckStackApp/locationindex/commit/30398ee094511362a08aad2279dbd1639ba87444))
* persist index metadata and remove global tuning state ([d674373](https://github.com/RuckStackApp/locationindex/commit/d674373c4c015251571451ed3e46fb49113efd9b))


### Bug Fixes

* harden persisted index writes and corruption checks ([db34d22](https://github.com/RuckStackApp/locationindex/commit/db34d222cfce41d7ef0aeb3f631e45e2ef189be1))
* improve query performance and adaptive spatial indexing ([40c4dde](https://github.com/RuckStackApp/locationindex/commit/40c4ddeac6a196b3311dcb66792c0a4542caad6c))
* optimize compressed posting loads ([0ec8d92](https://github.com/RuckStackApp/locationindex/commit/0ec8d9284095b4bda54bff955e4be1f3540765f7))
