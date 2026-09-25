package gateway

import (
	"regexp"
	"strings"
)

// Azure OpenAI routes (docs/model-gateway-plan.md §2, §4): the same OpenAI
// request, sent to the organization's own Azure resource
// (https://<resource>.openai.azure.com), to the model's deployment, with the
// route's api-version and the resource key in the api-key header. The
// resource is a name, never a URL, so a route cannot point the gateway at
// an arbitrary host.

var azureResource = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

func validAzureResource(name string) bool { return azureResource.MatchString(name) }

// validAzureKey checks the stored key's shape; it is never logged.
func validAzureKey(key string) bool {
	return len(key) >= 16 && len(key) <= 256 && !strings.ContainsAny(key, " \t\r\n\x00")
}

// ParseAzureKey validates an Azure OpenAI key before it is sealed.
func ParseAzureKey(raw []byte) (string, bool) {
	key := strings.TrimSpace(string(raw))
	return key, validAzureKey(key)
}
