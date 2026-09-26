## 0.0.1 (Unreleased)

BREAKING CHANGES:

* resource/relay_postgresql_cluster: node groups take a required `role` of `electable` or `read_only`; the map key is now a free label, and the per-group `synchronous` and `failover` booleans are gone

ENHANCEMENTS:

* resource/relay_postgresql_cluster: node groups take a catalog `tier`; CPU and memory are computed from it
* resource/relay_postgresql_cluster: `resources.storage_autoscale` grows a group's volumes past `storage`, up to `max_storage`; the platform-reported size is `storage_status`
* data-source/relay_postgresql_cluster: exposes `storage_status`
* resource/relay_postgresql_user: `roles` accepts unknown values, so a user can reference a cluster created in the same apply

FEATURES:

* **New Data Source:** `relay_organization`

* **New Resource:** `relay_workspace`
* **New Data Source:** `relay_workspace`
* **New Resource:** `relay_connection`
* **New Resource:** `relay_postgresql_cluster`
* **New Data Source:** `relay_postgresql_cluster`
* **New Resource:** `relay_postgresql_user`
