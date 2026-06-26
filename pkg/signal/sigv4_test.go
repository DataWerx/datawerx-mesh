package signal

import "testing"

// TestSigV4Vector validates the signing algorithm against AWS's published SigV4
// test-suite "get-vanilla" example, so the hand-rolled signer is known-correct
// independent of any AWS SDK.
func TestSigV4Vector(t *testing.T) {
	creds := AWSCredentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	canonicalHeaders := "host:example.amazonaws.com\nx-amz-date:20150830T123600Z\n"
	got := sigV4Authorization(
		"GET", "/", "", canonicalHeaders, "host;x-amz-date", hexHash(nil),
		creds, "us-east-1", "service", "20150830T123600Z", "20150830",
	)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got != want {
		t.Fatalf("signature mismatch\n got: %s\nwant: %s", got, want)
	}
}
