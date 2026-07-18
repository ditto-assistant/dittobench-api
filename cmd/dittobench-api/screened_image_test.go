package main

import "testing"

func TestValidateScreenedImage(t *testing.T) {
	valid := submitRequest{
		TarballURL:          "https://example.com/source.tar.gz",
		ScreenedImageURL:    "https://example.com/image.tar.gz",
		ScreenedImageSHA256: "ab" + repeat("0", 62),
		ScreenedImageID:     "sha256:" + repeat("1", 64),
		ScreenedImageRef:    "ditto-screen/550e8400-e29b-41d4-a716-446655440000:latest",
		ScreenedImageSize:   123,
	}
	if msg := validateScreenedImage(valid); msg != "" {
		t.Fatalf("valid screened image rejected: %s", msg)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*submitRequest)
	}{
		{"uppercase image id", func(req *submitRequest) { req.ScreenedImageID = "sha256:" + repeat("A", 64) }},
		{"empty ref id", func(req *submitRequest) { req.ScreenedImageRef = "ditto-screen/:latest" }},
		{"nested ref", func(req *submitRequest) {
			req.ScreenedImageRef = "ditto-screen/nested/550e8400-e29b-41d4-a716-446655440000:latest"
		}},
		{"noncanonical uuid", func(req *submitRequest) {
			req.ScreenedImageRef = "ditto-screen/550E8400-E29B-41D4-A716-446655440000:latest"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := valid
			tc.mutate(&req)
			if msg := validateScreenedImage(req); msg == "" {
				t.Fatal("malformed screened image metadata accepted")
			}
		})
	}
	valid.TarballURL = ""
	if msg := validateScreenedImage(valid); msg == "" {
		t.Fatal("screened image without source tarball was accepted")
	}
}

func TestValidateScreenedImageAccess(t *testing.T) {
	req := submitRequest{ScreenedImageURL: "https://example.com/image.tar"}
	if msg := validateScreenedImageAccess(req, false); msg == "" {
		t.Fatal("public practice API accepted the screened-image bypass")
	}
	if msg := validateScreenedImageAccess(req, true); msg != "" {
		t.Fatalf("validator sandbox rejected screened image: %s", msg)
	}
	if msg := validateScreenedImageAccess(submitRequest{}, false); msg != "" {
		t.Fatalf("legacy source-build request rejected: %s", msg)
	}
}

func TestValidateScreenedImageRequired(t *testing.T) {
	if msg := validateScreenedImageRequired(submitRequest{TarballURL: "https://example.com/source.tgz"}, true); msg == "" {
		t.Fatal("validator policy allowed an untrusted source build")
	}
	if msg := validateScreenedImageRequired(submitRequest{ScreenedImageURL: "https://example.com/image.tar"}, true); msg != "" {
		t.Fatalf("screened image rejected: %s", msg)
	}
	if msg := validateScreenedImageRequired(submitRequest{TarballURL: "https://example.com/source.tgz"}, false); msg != "" {
		t.Fatalf("practice source build rejected: %s", msg)
	}
}

func repeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
