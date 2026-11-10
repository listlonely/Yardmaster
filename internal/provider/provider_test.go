package provider

import "testing"

func TestAccountValidateMutualExclusion(t *testing.T) {
	cases := []struct {
		name    string
		account Account
		wantErr bool
	}{
		{"api_key 默认形态", Account{Name: "a", APIKey: "sk-x"}, false},
		{"api_key 空值允许（现状兼容）", Account{Name: "a"}, false},
		{"oauth 正常", Account{Name: "a", Auth: AuthOAuth, OAuth: &OAuthMeta{State: OAuthStateOK}}, false},
		{"oauth 与 api_key 互斥", Account{Name: "a", APIKey: "sk-x", Auth: AuthOAuth}, true},
		{"api_key 携带 oauth 元数据", Account{Name: "a", APIKey: "sk-x", OAuth: &OAuthMeta{}}, true},
		{"未知 auth 形态", Account{Name: "a", Auth: "jwt"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.account.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
