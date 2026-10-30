## Unreleased

ENHANCEMENTS:

* resource/relay_postgresql_cluster, data-source/relay_postgresql_cluster: `shared_host` is the previous shared address of a database upgraded from the Free plan, which keeps working next to the new names; `endpoint_host`, `endpoint_port` and `public_endpoints` keep only the primary names and never an alias

BUG FIXES:

* resource/relay_postgresql_cluster: resizing a node group's `tier` no longer fails with "Provider produced inconsistent result after apply"; `cpu` and `memory` are recomputed from the new tier

## 0.0.2 (October 29, 2026)

ENHANCEMENTS:

* resource/relay_postgresql_database: `schemas` creates schemas inside the database, and user roles on the database cover them alongside `public`
* resource/relay_postgresql_database: `extensions` creates allow-listed extensions, each in `public` or one of `schemas`, with no SQL from the user
* resource/relay_postgresql_database: `lock_public` revokes the `public` schema from every role, and user roles stop covering it
* resource/relay_postgresql_user: the `dbAdmin` role runs DDL in its database's `public` and declared schemas and can create trusted extensions, while `readWrite` stays DML-only there
* resource/relay_postgresql_user: `roles` takes several roles across databases, and `roles[].schema` narrows a role to one schema

## 0.0.1 (October 28, 2026)

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
* resource/relay_postgresql_cluster, resource/relay_valkey_cluster: reading a cluster of the other engine, as an import of the wrong ID does, fails instead of filling state
* resource/relay_backup_auto_export: destroy deletes the export instead of disabling it, so the cloud integration it names is destroyed in the same run
* resource/relay_valkey_cluster: `storage` is optional and a minimum, and `max_storage` follows it; the sizes the platform raises them to no longer fail the apply or plan a shrink, and "Storage cannot shrink" no longer applies

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
* **New Resource:** `relay_valkey_cluster`
* **New Resource:** `relay_valkey_user`
* **New Data Source:** `relay_valkey_cluster`
* **New Resource:** `relay_backup_schedule`
* **New Data Source:** `relay_cloud_integration`
* **New Resource:** `relay_backup_auto_export`
* **New Resource:** `relay_cloud_integration`
* **New Data Source:** `relay_cloud_integration_setup`
