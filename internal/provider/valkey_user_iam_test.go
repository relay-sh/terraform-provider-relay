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

func valkeyIAMUserConfig(f *fakeServer, arn string) string {
	return valkeyUserConfig(f, "orders-app", "readWrite", fmt.Sprintf("  auth_method   = \"aws_iam\"\n  principal_arn = %q", arn))
}

func TestValkeyUserResource_AWSIAM(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: valkeyIAMUserConfig(f, ordersRoleARN),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_user.this", "auth_method", "aws_iam"),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "principal_arn", ordersRoleARN),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "generated_password", ""),
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
				Config: valkeyIAMUserConfig(f, billingRoleARN),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_valkey_user.this", plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_user.this", "principal_arn", billingRoleARN),
					func(*terraform.State) error {
						if u := f.lastUserUpdate; u.GetPrincipalArn() != billingRoleARN {
							return fmt.Errorf("update request = %+v", u)
						}
						return nil
					},
				),
			},
			{
				Config: valkeyUserConfig(f, "orders-app", "readWrite", `  password = "x"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_valkey_user.this", plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_user.this", "auth_method", "password"),
					resource.TestCheckNoResourceAttr("relay_valkey_user.this", "principal_arn"),
				),
			},
		},
	})
}

func TestValkeyUserResource_ImportAWSIAM(t *testing.T) {
	f := startFakeServer(t)
	f.users[userKey("db-1", "orders-app")] = &serverv1.DatabaseUser{
		Username:     "orders-app",
		AuthMethod:   "AWS_IAM",
		PrincipalArn: ordersRoleARN,
		Roles:        []*serverv1.DatabaseUserRole{{Role: "readWrite"}},
		Phase:        "Ready",
		CreatedAt:    timestamppb.New(time.Unix(1_700_000_000, 0)),
	}
	config := valkeyIAMUserConfig(f, ordersRoleARN)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       "relay_valkey_user.this",
				ImportState:        true,
				ImportStateId:      dbWorkspace + "/db-1/orders-app",
				ImportStatePersist: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_valkey_user.this", plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_user.this", "auth_method", "aws_iam"),
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

func TestValkeyUserResource_AWSIAMConfigGuards(t *testing.T) {
	f := startFakeServer(t)
	iam := fmt.Sprintf("  auth_method = \"aws_iam\"\n  principal_arn = %q\n", ordersRoleARN)
	cases := map[string]struct{ body, want string }{
		"no principal":      {`  auth_method = "aws_iam"`, "Missing principal_arn"},
		"iam with password": {iam + `  password = "x"`, "Password on an aws_iam user"},
		"iam with generate": {iam + `  generate_password = true`, "Password on an aws_iam user"},
		"arn on password":   {fmt.Sprintf("  password = \"x\"\n  principal_arn = %q", ordersRoleARN), "principal_arn on a password user"},
		"unknown method":    {`  auth_method = "certificate"`, "value must be one of"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      valkeyUserConfig(f, "orders-app", "readWrite", tc.body),
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}

func TestValkeyUserResource_UnknownAuthMethodPlans(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: "resource \"terraform_data\" \"later\" {\n  input = \"aws_iam\"\n}\n" +
				valkeyUserConfig(f, "orders-app", "readWrite", fmt.Sprintf("  auth_method = terraform_data.later.output\n  principal_arn = %q", ordersRoleARN)),
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		}},
	})
}
