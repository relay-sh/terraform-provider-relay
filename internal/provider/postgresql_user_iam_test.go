package provider

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const (
	ordersRoleARN  = "arn:aws:iam::123456789012:role/orders-api"
	billingRoleARN = "arn:aws:iam::123456789012:role/billing-api"
)

func iamUserConfig(f *fakeServer, arn string) string {
	return userConfig(f, "orders_app", fmt.Sprintf("  auth_method   = \"aws_iam\"\n  principal_arn = %q", arn))
}

func TestPostgreSQLUserResource_AWSIAM(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: iamUserConfig(f, ordersRoleARN),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_postgresql_user.this", "auth_method", "aws_iam"),
					resource.TestCheckResourceAttr("relay_postgresql_user.this", "principal_arn", ordersRoleARN),
					resource.TestCheckResourceAttr("relay_postgresql_user.this", "generated_password", ""),
					func(*terraform.State) error {
						c := f.lastUserCreate
						if c.GetAuthMethod() != "aws_iam" || c.GetPrincipalArn() != ordersRoleARN || c.GetPassword() != "" || c.GetGeneratePassword() {
							return fmt.Errorf("create request = %+v", c)
						}
						return nil
					},
				),
			},
			{
				Config: iamUserConfig(f, billingRoleARN),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_postgresql_user.this", plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_postgresql_user.this", "principal_arn", billingRoleARN),
					func(*terraform.State) error {
						if u := f.lastUserUpdate; u.GetPrincipalArn() != billingRoleARN {
							return fmt.Errorf("update request = %+v", u)
						}
						return nil
					},
				),
			},
			{
				Config: userConfig(f, "orders_app", `  password = "x"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_postgresql_user.this", plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.TestCheckNoResourceAttr("relay_postgresql_user.this", "principal_arn"),
			},
		},
	})
}

func TestPostgreSQLUserResource_ImportAWSIAM(t *testing.T) {
	f := startFakeServer(t)
	f.users[userKey("db-1", "orders_app")] = &serverv1.DatabaseUser{
		Username:     "orders_app",
		AuthMethod:   "aws_iam",
		PrincipalArn: ordersRoleARN,
		Roles:        []*serverv1.DatabaseUserRole{{Role: "readWrite", Db: "app"}},
		Phase:        "Ready",
		CreatedAt:    timestamppb.New(time.Unix(1_700_000_000, 0)),
	}
	config := iamUserConfig(f, ordersRoleARN)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       "relay_postgresql_user.this",
				ImportState:        true,
				ImportStateId:      dbWorkspace + "/db-1/orders_app",
				ImportStatePersist: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_postgresql_user.this", plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_postgresql_user.this", "principal_arn", ordersRoleARN),
					func(*terraform.State) error {
						if n := len(f.callsTo("CreateDatabaseUser")); n != 0 {
							return fmt.Errorf("an imported IAM user must not be recreated, got %d creates", n)
						}
						if f.lastUserUpdate.PrincipalArn != nil {
							return fmt.Errorf("an unchanged ARN must not be sent: %+v", f.lastUserUpdate)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestPostgreSQLUserResource_AWSIAMConfigGuards(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config:      userConfig(f, "orders_app", `  auth_method = "aws_iam"`),
				ExpectError: regexp.MustCompile("Missing principal_arn"),
			},
			{
				Config:      userConfig(f, "orders_app", fmt.Sprintf("  auth_method = \"aws_iam\"\n  principal_arn = %q\n  password = \"x\"", ordersRoleARN)),
				ExpectError: regexp.MustCompile("Password on an aws_iam user"),
			},
			{
				Config:      userConfig(f, "orders_app", fmt.Sprintf("  auth_method = \"aws_iam\"\n  principal_arn = %q\n  generate_password = true", ordersRoleARN)),
				ExpectError: regexp.MustCompile("Password on an aws_iam user"),
			},
			{
				Config:      userConfig(f, "orders_app", fmt.Sprintf("  password = \"x\"\n  principal_arn = %q", ordersRoleARN)),
				ExpectError: regexp.MustCompile("principal_arn on a password user"),
			},
			{
				Config:      userConfig(f, "orders_app", `  auth_method = "certificate"`),
				ExpectError: regexp.MustCompile("value must be one of"),
			},
		},
	})
}
