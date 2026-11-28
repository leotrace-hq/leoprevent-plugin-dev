package wire

import "testing"

func TestDefinesSymbolFindsADefinitionInGoTypeScriptAndPython(t *testing.T) {
	// Arrange
	cases := []struct {
		content string
		symbol  string
		want    bool
	}{
		{"package reviewstore\n\nfunc LoadFromMongo(ctx context.Context) error {", "LoadFromMongo", true},
		{"func (c Client) send(method string) {", "Client.send", true},
		{"export async function verifyCode(email: string) {", "verifyCode", true},
		{"export const parseBudget = (raw: string) => {", "parseBudget()", true},
		{"def safe_redirect(v):\n    return v", "safe_redirect", true},
		{"const x = verifyCode(email);", "verifyCode", false},
		{"func LoadFromMongoAll() {}", "LoadFromMongo", false},
		{"anything", "a b", false},
	}

	for _, c := range cases {
		// Act
		got := DefinesSymbol(c.content, c.symbol)

		// Assert
		if got != c.want {
			t.Errorf("DefinesSymbol(%q, %q) = %v, want %v", c.content, c.symbol, got, c.want)
		}
	}
}
