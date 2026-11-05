## 0.1.0 (November 4, 2026)

BREAKING CHANGES:

* resource/relay_postgresql_cluster, resource/relay_valkey_cluster: volume size and storage autoscaling are cluster-wide, because every node holds the same data. `nodes.<group>.resources.storage` and `nodes.<group>.resources.storage_autoscale` are removed in favour of a root `storage = { size, autoscale = { enabled, max_storage, threshold_percent, increase_percent } }`. To migrate, move the values to the root `storage` block and delete them from every group; `disk_type`, `iops` and `throughput` stay per group. State upgrades automatically: `storage.size` takes the largest per-group size and `storage.autoscale` the primary group's, with the largest `max_storage` of any group. Omit `storage` to take the platform's default size ([#115](https://github.com/relay-sh/terraform-provider-relay/pull/115))
* resource/relay_postgresql_cluster, resource/relay_valkey_cluster, data-source/relay_postgresql_cluster, data-source/relay_valkey_cluster: `storage_status` is a single object `{ size, last_expanded_at }` instead of a list per group, and is null until autoscaling grows the volumes. Replace `storage_status[0].size` with `storage_status.size` ([#115](https://github.com/relay-sh/terraform-provider-relay/pull/115))
* data-source/relay_valkey_cluster: `nodes.<group>.resources.storage` and `nodes.<group>.resources.storage_autoscale` are replaced by the root `storage` ([#115](https://github.com/relay-sh/terraform-provider-relay/pull/115))

FEATURES:

* data-source/relay_postgresql_cluster, data-source/relay_valkey_cluster: `storage` exposes the cluster's volume size and autoscaling ([#115](https://github.com/relay-sh/terraform-provider-relay/pull/115))

## 0.0.15 (November 4, 2026)

FEATURES:

* resource/relay_access_grant: new resource that gives a user or a team access to a connection or a connection group, optionally until `expires_at`. A grant that expired or was revoked outside Terraform is removed from state on refresh and recreated on the next apply ([#112](https://github.com/relay-sh/terraform-provider-relay/pull/112))

BUG FIXES:

* resource/relay_connection: on a connection created for a database user, changing `managed_database_id`, `user`, `database` or `engine`, or switching between a database user and a `host`, replaces the connection, because the platform built it from them and the server rejects those changes in place. `agent_pool` still updates in place ([#109](https://github.com/relay-sh/terraform-provider-relay/pull/109))

## 0.0.14 (November 4, 2026)

FEATURES:

* resource/relay_connection: `managed_database_id` with `user` and no `host` creates the connection on that PostgreSQL database user of the managed database, with `database` as the logical database. The platform sets the address, CA and credential, so `host`, `credential_ref`, `credential_source`, `password`, `tls_cert`, `tls_key`, `tls_ca` and `host_tls_ca` are rejected at plan time in this mode. Needs the `database_user_connections` feature for the organization. `host` is now optional, and a connection without a database user still requires it
* resource/relay_connection: `database_user` exposes the database user the platform bound the connection to

## 0.0.13 (November 4, 2026)

BREAKING CHANGES:

* resource/relay_workspace, data-source/relay_workspace: `is_default` is removed, after its deprecation in 0.0.12. State that still holds it loads without error; remove the attribute from configurations that reference it ([#101](https://github.com/relay-sh/terraform-provider-relay/pull/101))

FEATURES:

* resource/relay_postgresql_database: `deletion_protection` blocks destroying the logical database, including the destroy half of a replacement, while it is `true`. A protected database also blocks deleting its cluster ([#103](https://github.com/relay-sh/terraform-provider-relay/pull/103))
* resource/relay_connection: `deletion_protection` blocks destroying the connection, including the destroy half of a replacement, while it is `true`. A protected connection also blocks deleting its cluster ([#104](https://github.com/relay-sh/terraform-provider-relay/pull/104))
* resource/relay_workspace, data-source/relay_workspace: `deletion_protection` blocks destroying the workspace while it is `true`. A protected workspace also blocks deleting its organization ([#105](https://github.com/relay-sh/terraform-provider-relay/pull/105))

## 0.0.12 (November 4, 2026)

FEATURES:

* resource/relay_postgresql_cluster, resource/relay_valkey_cluster: `deletion_protection` blocks destroying the cluster, including the destroy half of a replacement, while it is `true`. Turning it off needs the `org.deletion_protection.disable` permission. Turning it on needs the platform's `deletion_protection` feature for the organization ([#99](https://github.com/relay-sh/terraform-provider-relay/pull/99))

DEPRECATIONS:

* resource/relay_workspace, data-source/relay_workspace: `is_default` is deprecated and always `false`, because the platform no longer has a default workspace. It will be removed in the next release ([#98](https://github.com/relay-sh/terraform-provider-relay/pull/98))

## 0.0.11 (November 4, 2026)

BREAKING CHANGES:

* resource/relay_backup_schedule: `pitr_window_days` accepts 1 to 7 for every engine; a value above 7 is rejected at plan time. The platform restores to any instant only within the last 7 days ([#96](https://github.com/relay-sh/terraform-provider-relay/pull/96))

## 0.0.10 (November 3, 2026)

FEATURES:

* resource/relay_postgresql_cluster, resource/relay_valkey_cluster: `default_connections` creates the Relay-managed read-only, read-write and admin users, each with a connection, as the Relay console does. Omitted on create, all three are created. Changing a role on an existing cluster creates it, or deletes its connection and then its user, without replacing the cluster ([#92](https://github.com/relay-sh/terraform-provider-relay/pull/92), [#93](https://github.com/relay-sh/terraform-provider-relay/pull/93))

NOTES:

* resource/relay_postgresql_cluster, resource/relay_valkey_cluster: a cluster created without `default_connections` in its configuration now gets all three default connections. Set the roles to `false` to keep creating clusters without them

## 0.0.9 (November 2, 2026)

BUG FIXES:

* resource/relay_api_key: an imported key plans no change: the import reads `allowed_cidrs` and `expires_in` back from the key, leaving them null when they were copied from a parent key, so applying after an import no longer restarts the key's expiry ([#56](https://github.com/relay-sh/terraform-provider-relay/issues/56))
* resource/relay_api_key: writing the same `expires_in` another way, such as `1440m` for `24h`, no longer restarts the key's expiry

## 0.0.8 (November 1, 2026)

BUG FIXES:

* resource/relay_postgresql_cluster, resource/relay_valkey_cluster: an apply waits for the platform's in-flight operations, such as a rolling restart, instead of returning while the cluster still runs the old spec; a running backup does not hold it

## 0.0.7 (November 1, 2026)

BUG FIXES:

* resource/relay_valkey_cluster: changing a node group's `tier` no longer fails with "Provider produced inconsistent result after apply"; `cpu` and `memory` are recomputed from the new tier

## 0.0.6 (November 1, 2026)

BUG FIXES:

* resource/relay_postgresql_cluster, resource/relay_valkey_cluster: `restore.target_time` written with another offset for the same instant no longer replaces the cluster; the plan keeps the stored value
* resource/relay_valkey_cluster: a restore whose configuration changes the nodes no longer fails with "node resources.storage is required" when `storage` is not set

## 0.0.5 (November 1, 2026)

ENHANCEMENTS:

* resource/relay_postgresql_cluster, resource/relay_valkey_cluster: `restore` creates the cluster from another cluster's backups, the latest one or a point in time with `target_time`; a Valkey `target_time` needs `replication` mode. The provider applies the configured nodes and settings over the source's after the restore; changing `restore` replaces the cluster

## 0.0.4 (October 31, 2026)

ENHANCEMENTS:

* resource/relay_backup_schedule: `pitr_window_days` turns on point-in-time restore for a Valkey cluster in `replication` mode, 1-35 days; a cluster-mode cluster still rejects it

## 0.0.3 (October 30, 2026)

ENHANCEMENTS:

* resource/relay_postgresql_database: `extensions[].schema` may name a schema outside `schemas`; the platform creates it readable by every role and writable by none, and user roles find its operators and functions without qualifying them
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
