package memory

import "testing"

func TestLooksSensitive(t *testing.T) {
	sensitive := []string{
		"用户的密码是 abc12345",
		"登录口令: P@ssw0rd!",
		"api_key = sk-ABCDEFGHIJKLMNOP1234",
		"OpenAI key sk-abcdefghijklmnop12345",
		"GitHub token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345",
		"用户身份证 11010119900307123X",
		"银行卡号 6222021234567890123",
		"AWS AKIAIOSFODNN7EXAMPLE 是访问密钥",
		"-----BEGIN RSA PRIVATE KEY-----",
	}
	for _, s := range sensitive {
		if !LooksSensitive(s) {
			t.Errorf("应判定为敏感: %q", s)
		}
	}

	benign := []string{
		"用户喜欢用密码管理器",         // 「密码」无赋值上下文，不误杀
		"用户在 1Password 团队工作", // 提到密码工具但非凭证
		"用户住在北京",
		"用户的手机号备注为常用联系方式", // 不含 11/13-19 位裸数字
		"用户偏好简洁的代码风格",
		"项目用 Go + Vue 3",
	}
	for _, s := range benign {
		if LooksSensitive(s) {
			t.Errorf("不应误判为敏感: %q", s)
		}
	}
}
