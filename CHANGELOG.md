## 0.0.1 (Unreleased)

BREAKING CHANGES:

* resource/relay_postgresql_cluster: node groups take a required `role` of `electable` or `read_only`; the map key is now a free label, and the per-group `synchronous` and `failover` booleans are gone
* resource/relay_postgresql_cluster: every node group requires `cloud` and `region`; a group that omitted them was placed on the first matching provisioner

ENHANCEMENTS:

* resource/relay_postgresql_cluster: node groups take a catalog `tier`; CPU and memory are computed from it
* resource/relay_postgresql_cluster: `resources.storage_autoscale` grows a group's volumes past `storage`, up to `max_storage`; the platform-reported size is `storage_status`
* data-source/relay_postgresql_cluster: exposes `storage_status`
* resource/relay_postgresql_user: `roles` accepts unknown values, so a user can reference a cluster created in the same apply

BUG FIXES:

* resource/relay_postgresql_cluster, data-source/relay_postgresql_cluster: `endpoint_host` and `endpoint_port` are the public read-write endpoint, no longer the cluster's in-cluster address
* resource/relay_connection: a connection with `managed_database_id` keeps its configured `host`; the API no longer returns it

FEATURES:

* **New Data Source:** `relay_organization`

* **New Resource:** `relay_workspace`
* **New Data Source:** `relay_workspace`
* **New Resource:** `relay_api_key`
* **New Data Source:** `relay_api_key`
* **New Resource:** `relay_connection`
* **New Resource:** `relay_postgresql_cluster`
* **New Data Source:** `relay_postgresql_cluster`
* **New Resource:** `relay_postgresql_user`
* **New Resource:** `relay_postgresql_database`
* **New Resource:** `relay_ip_access_list`
* **New Data Source:** `relay_ip_access_list`
* **New Resource:** `relay_private_endpoint_service`
* **New Data Source:** `relay_private_endpoint_service`
* **New Resource:** `relay_private_endpoint`
* **New Data Source:** `relay_private_endpoint`
* **New Data Source:** `relay_regions`
* **New Data Source:** `relay_cloud_providers`
