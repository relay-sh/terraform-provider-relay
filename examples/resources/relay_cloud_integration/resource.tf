data "relay_cloud_integration_setup" "aws" {
  workspace_id = relay_workspace.prod.id
}

resource "aws_s3_bucket" "backups" {
  bucket = "acme-relay-backups"
}

resource "aws_iam_role" "relay_backups" {
  name               = "relay-backups"
  assume_role_policy = data.relay_cloud_integration_setup.aws.trust_policy
}

resource "aws_iam_role_policy" "relay_backups" {
  role   = aws_iam_role.relay_backups.id
  policy = replace(data.relay_cloud_integration_setup.aws.permissions_policy, "YOUR_BUCKET", aws_s3_bucket.backups.bucket)
}

resource "relay_cloud_integration" "backups" {
  workspace_id = relay_workspace.prod.id
  name         = "backups"
  role_arn     = aws_iam_role.relay_backups.arn
}

resource "relay_backup_auto_export" "prod" {
  workspace_id   = relay_workspace.prod.id
  integration_id = relay_cloud_integration.backups.id
  bucket         = aws_s3_bucket.backups.bucket
  prefix         = "prod/"

  depends_on = [aws_iam_role_policy.relay_backups]
}
