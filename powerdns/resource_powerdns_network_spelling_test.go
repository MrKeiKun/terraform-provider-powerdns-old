package powerdns

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

func TestAccPDNSNetworkEquivalentCIDRUpdate(t *testing.T) {
	const resourceName = "powerdns_network.spelling"
	const expanded = "2001:0db8:0000::/32"
	const compressed = "2001:db8::/32"
	var originalID string

	config := func(cidr string) string {
		return fmt.Sprintf(`
resource "powerdns_zone" "spelling" {
  name = "network-spelling-regression.example."
  kind = "Native"
}

resource "powerdns_view_zone_association" "spelling" {
  view = "network-spelling-regression"
  zone = powerdns_zone.spelling.name
}

resource "powerdns_network" "spelling" {
  network = %q
  view    = powerdns_view_zone_association.spelling.view
}
`, cidr)
	}

	resource.ParallelTest(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheckViews(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config(expanded),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "network", expanded),
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttrWith(resourceName, "id", func(id string) error {
						originalID = id
						return nil
					}),
				),
			},
			{
				Config: config(compressed),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "network", compressed),
					resource.TestCheckResourceAttrWith(resourceName, "id", func(id string) error {
						if id != originalID {
							return fmt.Errorf("resource ID changed from %q to %q", originalID, id)
						}
						return nil
					}),
				),
			},
			{
				Config:   config(compressed),
				PlanOnly: true,
			},
		},
	})
}
