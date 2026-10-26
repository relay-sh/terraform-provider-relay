resource "relay_valkey_cluster" "sessions" {
  workspace_id = relay_workspace.prod.id
  name         = "sessions"
  version      = "9.1"

  nodes = {
    cache = {
      count  = 2
      cloud  = "aws"
      region = "us-east-1"
      tier   = "RC20"
      resources = {
        storage = "8Gi"
      }
    }
  }

  parameters = {
    "maxmemory-policy" = "allkeys-lru"
  }
}

resource "relay_valkey_cluster" "leaderboard" {
  workspace_id       = relay_workspace.prod.id
  name               = "leaderboard"
  mode               = "cluster"
  shards             = 3
  replicas_per_shard = 1

  nodes = {
    shards = {
      count  = 6
      cloud  = "aws"
      region = "us-east-1"
      tier   = "RC40"
      resources = {
        storage = "28Gi"
        storage_autoscale = {
          enabled     = true
          max_storage = "100Gi"
        }
      }
    }
  }

  lifecycle {
    prevent_destroy = true
  }
}
