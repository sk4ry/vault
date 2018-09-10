package awsauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/hashicorp/vault/logical"
	"github.com/y0ssar1an/q"
)

func TestBackend_pathLogin_getCallerIdentityResponse(t *testing.T) {
	responseFromUser := `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <GetCallerIdentityResult>
    <Arn>arn:aws:iam::123456789012:user/MyUserName</Arn>
    <UserId>ASOMETHINGSOMETHINGSOMETHING</UserId>
    <Account>123456789012</Account>
  </GetCallerIdentityResult>
  <ResponseMetadata>
    <RequestId>7f4fc40c-853a-11e6-8848-8d035d01eb87</RequestId>
  </ResponseMetadata>
</GetCallerIdentityResponse>`
	expectedUserArn := "arn:aws:iam::123456789012:user/MyUserName"

	responseFromAssumedRole := `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <GetCallerIdentityResult>
  <Arn>arn:aws:sts::123456789012:assumed-role/RoleName/RoleSessionName</Arn>
  <UserId>ASOMETHINGSOMETHINGELSE:RoleSessionName</UserId>
    <Account>123456789012</Account>
  </GetCallerIdentityResult>
  <ResponseMetadata>
    <RequestId>7f4fc40c-853a-11e6-8848-8d035d01eb87</RequestId>
  </ResponseMetadata>
</GetCallerIdentityResponse>`
	expectedRoleArn := "arn:aws:sts::123456789012:assumed-role/RoleName/RoleSessionName"

	parsedUserResponse, err := parseGetCallerIdentityResponse(responseFromUser)
	if err != nil {
		t.Fatal(err)
	}
	if parsedArn := parsedUserResponse.GetCallerIdentityResult[0].Arn; parsedArn != expectedUserArn {
		t.Errorf("expected to parse arn %#v, got %#v", expectedUserArn, parsedArn)
	}

	parsedRoleResponse, err := parseGetCallerIdentityResponse(responseFromAssumedRole)
	if err != nil {
		t.Fatal(err)
	}
	if parsedArn := parsedRoleResponse.GetCallerIdentityResult[0].Arn; parsedArn != expectedRoleArn {
		t.Errorf("expected to parn arn %#v; got %#v", expectedRoleArn, parsedArn)
	}

	_, err = parseGetCallerIdentityResponse("SomeRandomGibberish")
	if err == nil {
		t.Errorf("expected to NOT parse random giberish, but didn't get an error")
	}
}

func TestBackend_pathLogin_parseIamArn(t *testing.T) {
	testParser := func(inputArn, expectedCanonicalArn string, expectedEntity iamEntity) {
		entity, err := parseIamArn(inputArn)
		if err != nil {
			t.Fatal(err)
		}
		if expectedCanonicalArn != "" && entity.canonicalArn() != expectedCanonicalArn {
			t.Fatalf("expected to canonicalize ARN %q into %q but got %q instead", inputArn, expectedCanonicalArn, entity.canonicalArn())
		}
		if *entity != expectedEntity {
			t.Fatalf("expected to get iamEntity %#v from input ARN %q but instead got %#v", expectedEntity, inputArn, *entity)
		}
	}

	testParser("arn:aws:iam::123456789012:user/UserPath/MyUserName",
		"arn:aws:iam::123456789012:user/MyUserName",
		iamEntity{Partition: "aws", AccountNumber: "123456789012", Type: "user", Path: "UserPath", FriendlyName: "MyUserName"},
	)
	canonicalRoleArn := "arn:aws:iam::123456789012:role/RoleName"
	testParser("arn:aws:sts::123456789012:assumed-role/RoleName/RoleSessionName",
		canonicalRoleArn,
		iamEntity{Partition: "aws", AccountNumber: "123456789012", Type: "assumed-role", FriendlyName: "RoleName", SessionInfo: "RoleSessionName"},
	)
	testParser("arn:aws:iam::123456789012:role/RolePath/RoleName",
		canonicalRoleArn,
		iamEntity{Partition: "aws", AccountNumber: "123456789012", Type: "role", Path: "RolePath", FriendlyName: "RoleName"},
	)
	testParser("arn:aws:iam::123456789012:instance-profile/profilePath/InstanceProfileName",
		"",
		iamEntity{Partition: "aws", AccountNumber: "123456789012", Type: "instance-profile", Path: "profilePath", FriendlyName: "InstanceProfileName"},
	)

	// Test that it properly handles pathological inputs...
	_, err := parseIamArn("")
	if err == nil {
		t.Error("expected error from empty input string")
	}

	_, err = parseIamArn("arn:aws:iam::123456789012:role")
	if err == nil {
		t.Error("expected error from malformed ARN without a role name")
	}

	_, err = parseIamArn("arn:aws:iam")
	if err == nil {
		t.Error("expected error from incomplete ARN (arn:aws:iam)")
	}

	_, err = parseIamArn("arn:aws:iam::1234556789012:/")
	if err == nil {
		t.Error("expected error from empty principal type and no principal name (arn:aws:iam::1234556789012:/)")
	}
}

func TestBackend_validateVaultHeaderValue(t *testing.T) {
	const canaryHeaderValue = "Vault-Server"
	requestURL, err := url.Parse("https://sts.amazonaws.com/")
	if err != nil {
		t.Fatalf("error parsing test URL: %v", err)
	}
	postHeadersMissing := http.Header{
		"Host":          []string{"Foo"},
		"Authorization": []string{"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-vault-aws-iam-server-id, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"},
	}
	postHeadersInvalid := http.Header{
		"Host":            []string{"Foo"},
		iamServerIdHeader: []string{"InvalidValue"},
		"Authorization":   []string{"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-vault-aws-iam-server-id, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"},
	}
	postHeadersUnsigned := http.Header{
		"Host":            []string{"Foo"},
		iamServerIdHeader: []string{canaryHeaderValue},
		"Authorization":   []string{"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"},
	}
	postHeadersValid := http.Header{
		"Host":            []string{"Foo"},
		iamServerIdHeader: []string{canaryHeaderValue},
		"Authorization":   []string{"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-vault-aws-iam-server-id, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"},
	}

	postHeadersSplit := http.Header{
		"Host":            []string{"Foo"},
		iamServerIdHeader: []string{canaryHeaderValue},
		"Authorization":   []string{"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request", "SignedHeaders=content-type;host;x-amz-date;x-vault-aws-iam-server-id, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"},
	}

	err = validateVaultHeaderValue(postHeadersMissing, requestURL, canaryHeaderValue)
	if err == nil {
		t.Error("validated POST request with missing Vault header")
	}

	err = validateVaultHeaderValue(postHeadersInvalid, requestURL, canaryHeaderValue)
	if err == nil {
		t.Error("validated POST request with invalid Vault header value")
	}

	err = validateVaultHeaderValue(postHeadersUnsigned, requestURL, canaryHeaderValue)
	if err == nil {
		t.Error("validated POST request with unsigned Vault header")
	}

	err = validateVaultHeaderValue(postHeadersValid, requestURL, canaryHeaderValue)
	if err != nil {
		t.Errorf("did NOT validate valid POST request: %v", err)
	}

	err = validateVaultHeaderValue(postHeadersSplit, requestURL, canaryHeaderValue)
	if err != nil {
		t.Errorf("did NOT validate valid POST request with split Authorization header: %v", err)
	}
}

func TestBackend_pathLogin_parseIamRequestHeaders(t *testing.T) {
	testIamParser := func(headers interface{}, expectedHeaders http.Header) error {
		headersJSON, err := json.Marshal(headers)
		if err != nil {
			return fmt.Errorf("unable to JSON encode headers: %v", err)
		}
		headersB64 := base64.StdEncoding.EncodeToString(headersJSON)

		parsedHeaders, err := parseIamRequestHeaders(headersB64)
		if err != nil {
			return fmt.Errorf("error parsing encoded headers: %v", err)
		}
		if parsedHeaders == nil {
			return fmt.Errorf("nil result from parsing headers")
		}
		if !reflect.DeepEqual(parsedHeaders, expectedHeaders) {
			return fmt.Errorf("parsed headers not equal to input headers")
		}
		return nil
	}

	headersGoStyle := http.Header{
		"Header1": []string{"Value1"},
		"Header2": []string{"Value2"},
	}
	headersMixedType := map[string]interface{}{
		"Header1": "Value1",
		"Header2": []string{"Value2"},
	}

	err := testIamParser(headersGoStyle, headersGoStyle)
	if err != nil {
		t.Errorf("error parsing go-style headers: %v", err)
	}
	err = testIamParser(headersMixedType, headersGoStyle)
	if err != nil {
		t.Errorf("error parsing mixed-style headers: %v", err)
	}
}

func TestBackend_pathLogin_IAMHeaders(t *testing.T) {
	storage := &logical.InmemStorage{}
	config := logical.TestBackendConfig()
	config.StorageView = storage
	b, err := Backend(config)
	if err != nil {
		t.Fatal(err)
	}

	err = b.Setup(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}

	// configure client

	const testVaultHeaderValue = "VaultAcceptanceTesting"
	const testValidRoleName = "valid-role"

	responseFromUser := `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <GetCallerIdentityResult>
    <Arn>arn:aws:iam::123456789012:user/valid-role</Arn>
    <UserId>ASOMETHINGSOMETHINGSOMETHING</UserId>
    <Account>123456789012</Account>
  </GetCallerIdentityResult>
  <ResponseMetadata>
    <RequestId>7f4fc40c-853a-11e6-8848-8d035d01eb87</RequestId>
  </ResponseMetadata>
</GetCallerIdentityResponse>`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, responseFromUser)
	}))
	defer ts.Close()

	clientConfigData := map[string]interface{}{
		"iam_server_id_header_value": testVaultHeaderValue,
		"endpoint":                   ts.URL,
		"iam_endpoint":               ts.URL,
		"sts_endpoint":               ts.URL,
	}
	clientRequest := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/client",
		Storage:   storage,
		Data:      clientConfigData,
	}
	_, err = b.HandleRequest(context.Background(), clientRequest)
	if err != nil {
		t.Fatal(err)
	}

	// create a role entry
	// mutex here may not be needed, but was copied from another code example
	b.roleMutex.Lock()
	roleEntry, err := b.nonLockedAWSRole(context.Background(), storage, testValidRoleName)
	if err != nil {
		t.Fatalf("failed to get entry: %s", err)
	}
	b.roleMutex.Unlock()

	if roleEntry == nil {
		roleEntry = &awsRoleEntry{
			Version: currentRoleStorageVersion,
		}
	}
	roleEntry.AuthType = iamAuthType

	if err := b.nonLockedSetAWSRole(context.Background(), storage, testValidRoleName, roleEntry); err != nil {
		t.Fatalf("failed to set entry: %s", err)
	}

	awsSession, err := session.NewSession()
	if err != nil {
		fmt.Println("failed to create session,", err)
		return
	}

	stsService := sts.New(awsSession)
	stsInputParams := &sts.GetCallerIdentityInput{}

	stsRequestValid, _ := stsService.GetCallerIdentityRequest(stsInputParams)
	stsRequestValid.HTTPRequest.Header.Add(iamServerIdHeader, testVaultHeaderValue)
	stsRequestValid.HTTPRequest.Header.Add("Authorization", "AWS4-HMAC-SHA256 Date=2018-09-07, Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-vault-aws-iam-server-id, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7")
	stsRequestValid.Sign()

	loginData, err := buildCallerIdentityLoginData(stsRequestValid.HTTPRequest, testValidRoleName)
	if err != nil {
		t.Fatal(err)
	}

	q.Q("loginData=", loginData)

	loginRequest := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      loginData,
	}

	resp, err := b.HandleRequest(context.Background(), loginRequest)
	if err != nil || resp == nil || resp.IsError() {
		t.Errorf("bad: expected failed login due to invalid role: resp:%#v\nerr:%v", resp, err)
	}

}
