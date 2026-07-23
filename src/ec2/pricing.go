package ec2

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// pricingAPIRegion is where the AWS Price List Query API is served. It only has
// endpoints in us-east-1 and ap-south-1, regardless of the region being priced.
const pricingAPIRegion = "us-east-1"

// OnDemandPrice returns the hourly USD on-demand price for INSTANCETYPE in
// REGION (shared tenancy, Linux, no pre-installed software), or "" when AWS
// returns no matching offer.
//
// Requires the "pricing:GetProducts" IAM permission.
func OnDemandPrice(ctx context.Context, region, instanceType string) (string, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(pricingAPIRegion))
	if err != nil {
		return "", fmt.Errorf("aws config: %w", err)
	}
	term := pricingtypes.FilterTypeTermMatch
	out, err := pricing.NewFromConfig(cfg).GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		MaxResults:  aws.Int32(1),
		Filters: []pricingtypes.Filter{
			{Type: term, Field: aws.String("instanceType"), Value: aws.String(instanceType)},
			{Type: term, Field: aws.String("regionCode"), Value: aws.String(region)},
			{Type: term, Field: aws.String("operatingSystem"), Value: aws.String("Linux")},
			{Type: term, Field: aws.String("tenancy"), Value: aws.String("Shared")},
			{Type: term, Field: aws.String("preInstalledSw"), Value: aws.String("NA")},
			{Type: term, Field: aws.String("capacitystatus"), Value: aws.String("Used")},
		},
	})
	if err != nil {
		return "", err
	}
	for _, raw := range out.PriceList {
		if p := firstOnDemandUSD(raw); p != "" {
			return p, nil
		}
	}
	return "", nil
}

// firstOnDemandUSD digs the hourly USD figure out of one Price List document.
// The shape is terms.OnDemand.<offer>.priceDimensions.<dim>.pricePerUnit.USD,
// with opaque generated keys at both levels.
func firstOnDemandUSD(raw string) string {
	var doc struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return ""
	}
	for _, offer := range doc.Terms.OnDemand {
		for _, dim := range offer.PriceDimensions {
			if usd := dim.PricePerUnit["USD"]; usd != "" {
				return usd
			}
		}
	}
	return ""
}
