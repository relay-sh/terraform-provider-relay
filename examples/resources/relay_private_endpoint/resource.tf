resource "relay_private_endpoint_service" "use1" {
  workspace_id = relay_workspace.prod.id
  cloud        = "aws"
  region       = "us-east-1"
}

resource "aws_security_group" "relay" {
  name   = "relay-private-endpoint"
  vpc_id = aws_vpc.app.id

  ingress {
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.app.cidr_block]
  }
}

resource "aws_vpc_endpoint" "relay" {
  vpc_id             = aws_vpc.app.id
  service_name       = relay_private_endpoint_service.use1.endpoint_service_name
  vpc_endpoint_type  = "Interface"
  subnet_ids         = aws_subnet.private[*].id
  security_group_ids = [aws_security_group.relay.id]
}

resource "relay_private_endpoint" "app" {
  workspace_id    = relay_workspace.prod.id
  service_id      = relay_private_endpoint_service.use1.id
  vpc_endpoint_id = aws_vpc_endpoint.relay.id
}
