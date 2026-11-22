resource "relay_valkey_cluster" "sessions" {
  workspace_id = relay_workspace.prod.id
  name         = "sessions"
  version      = "9.1"

  storage = {
    size = "8Gi"
  }

  nodes = {
    cache = {
      count     = 2
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "RC20"
      resources = {}
      autoscaling = {
        enabled  = true
        min_tier = "RC10"
        max_tier = "RC40"
      }
    }
  }

  parameters = {
    "maxmemory-policy" = "allkeys-lru"
  }
}

resource "relay_valkey_cluster" "leaderboard" {
  workspace_id        = relay_workspace.prod.id
  name                = "leaderboard"
  mode                = "cluster"
  shards              = 3
  replicas_per_shard  = 1
  deletion_protection = true

  storage = {
    size = "28Gi"
    autoscale = {
      enabled     = true
      max_storage = "100Gi"
    }
  }

  nodes = {
    shards = {
      count     = 6
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "RC40"
      resources = {}
    }
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "relay_valkey_cluster" "orders" {
  workspace_id = relay_workspace.prod.id
  name         = "orders"

  nodes = {
    cache = {
      count     = 2
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "RC20"
      resources = {}
    }
  }

  distribution {
    mode = "standby"

    region {
      cloud   = "aws"
      region  = "us-east-1"
      primary = true
    }

    region {
      cloud  = "aws"
      region = "eu-west-1"
    }
  }
}

output "orders_standby_links" {
  value = { for r in relay_valkey_cluster.orders.regions : "${r.cloud}/${r.region}" => r.replication_link if r.role == "standby" }
}
