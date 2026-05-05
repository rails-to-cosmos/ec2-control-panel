package ec2

import (
	"embed"
	"encoding/base64"
	"fmt"
	"strings"
)

//go:embed templates/*.sh
var userDataFS embed.FS

func referenceUserData() (string, error) {
	raw, err := userDataFS.ReadFile("templates/user-data-reference.sh")
	if err != nil {
		return "", fmt.Errorf("read reference template: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func chainloadUserData(volumeID, awsAccessKey, awsSecret, awsRegion string) (string, error) {
	raw, err := userDataFS.ReadFile("templates/user-data-chainload.sh")
	if err != nil {
		return "", fmt.Errorf("read chainload template: %w", err)
	}
	r := strings.NewReplacer(
		"{{VOLUME_ID}}", volumeID,
		"{{AWS_ACCESS_KEY_ID}}", awsAccessKey,
		"{{AWS_SECRET_ACCESS_KEY}}", awsSecret,
		"{{AWS_REGION}}", awsRegion,
	)
	return base64.StdEncoding.EncodeToString([]byte(r.Replace(string(raw)))), nil
}
