package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// bucketReport says whether a remote's bucket is private, and who we are.
// Everything here is read-only.
type bucketReport struct {
	Bucket   string
	Region   string
	Identity string   // ARN of the credentials in use
	Public   []string // ways the bucket is, or could become, public
	Risks    []string // other things worth fixing (e.g. root credentials)
	Facts    []string // plain settings, for `remote check`
	Unknown  []string // checks we couldn't run (permissions, provider)
}

func (r *bucketReport) private() bool { return len(r.Public) == 0 }

// bucketAuditor is implemented by remotes that can inspect their bucket.
type bucketAuditor interface {
	auditBucket(ctx context.Context) *bucketReport
}

func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

func (s *s3Remote) auditBucket(ctx context.Context) *bucketReport {
	r := &bucketReport{Bucket: s.bucket, Region: s.awsCfg.Region}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b := aws.String(s.bucket)
	unknown := func(what string, err error) {
		r.Unknown = append(r.Unknown, fmt.Sprintf("%s (%s)", what, firstNonEmpty(apiCode(err), err.Error())))
	}
	if s.customEndpoint != "" {
		r.Unknown = append(r.Unknown, "privacy settings: "+s.customEndpoint+" isn't AWS S3 — check the provider's dashboard (for R2: the bucket's public r2.dev / custom-domain access)")
		return r
	}

	if id, err := sts.NewFromConfig(s.awsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err == nil {
		r.Identity = aws.ToString(id.Arn)
		if strings.HasSuffix(r.Identity, ":root") {
			r.Risks = append(r.Risks, "these are the AWS account's ROOT access keys — use an IAM user limited to this bucket instead")
		}
	} else {
		unknown("caller identity", err)
	}

	switch pab, err := s.client.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: b}); {
	case err == nil:
		c := pab.PublicAccessBlockConfiguration
		var off []string
		for name, on := range map[string]*bool{"BlockPublicAcls": c.BlockPublicAcls, "IgnorePublicAcls": c.IgnorePublicAcls,
			"BlockPublicPolicy": c.BlockPublicPolicy, "RestrictPublicBuckets": c.RestrictPublicBuckets} {
			if !aws.ToBool(on) {
				off = append(off, name)
			}
		}
		if len(off) == 0 {
			r.Facts = append(r.Facts, "Block Public Access: all on")
		} else {
			r.Public = append(r.Public, "Block Public Access is partly off ("+strings.Join(off, ", ")+") — a policy or ACL could make objects public")
		}
	case apiCode(err) == "NoSuchPublicAccessBlockConfiguration":
		r.Public = append(r.Public, "Block Public Access isn't configured for this bucket")
	default:
		unknown("Block Public Access", err)
	}

	switch st, err := s.client.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: b}); {
	case err == nil && st.PolicyStatus != nil && aws.ToBool(st.PolicyStatus.IsPublic):
		r.Public = append(r.Public, "the bucket policy makes it PUBLIC")
	case err == nil:
		r.Facts = append(r.Facts, "bucket policy: present, not public")
	case apiCode(err) == "NoSuchBucketPolicy":
		r.Facts = append(r.Facts, "bucket policy: none")
	default:
		unknown("bucket policy status", err)
	}

	if acl, err := s.client.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: b}); err == nil {
		for _, g := range acl.Grants {
			uri := aws.ToString(g.Grantee.URI)
			switch {
			case strings.HasSuffix(uri, "/global/AllUsers"):
				r.Public = append(r.Public, fmt.Sprintf("the ACL grants %s to EVERYONE", g.Permission))
			case strings.HasSuffix(uri, "/global/AuthenticatedUsers"):
				r.Public = append(r.Public, fmt.Sprintf("the ACL grants %s to ANY AWS account", g.Permission))
			}
		}
		r.Facts = append(r.Facts, fmt.Sprintf("ACL: %d grant(s)", len(acl.Grants)))
	} else {
		unknown("ACL", err)
	}

	if own, err := s.client.GetBucketOwnershipControls(ctx, &s3.GetBucketOwnershipControlsInput{Bucket: b}); err == nil {
		for _, rule := range own.OwnershipControls.Rules {
			r.Facts = append(r.Facts, "object ownership: "+string(rule.ObjectOwnership))
		}
	}

	switch _, err := s.client.GetBucketWebsite(ctx, &s3.GetBucketWebsiteInput{Bucket: b}); {
	case err == nil:
		r.Public = append(r.Public, "static website hosting is ON")
	case apiCode(err) == "NoSuchWebsiteConfiguration":
		r.Facts = append(r.Facts, "website hosting: off")
	default:
		unknown("website hosting", err)
	}

	if v, err := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: b}); err == nil {
		status := string(v.Status)
		if status == "" {
			status = "never enabled"
		}
		r.Facts = append(r.Facts, "versioning: "+status)
	}
	if e, err := s.client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: b}); err == nil {
		for _, rule := range e.ServerSideEncryptionConfiguration.Rules {
			if d := rule.ApplyServerSideEncryptionByDefault; d != nil {
				r.Facts = append(r.Facts, "server-side encryption: "+string(d.SSEAlgorithm)+" (shasync also encrypts client-side)")
			}
		}
	}
	return r
}

// checkBucketBeforeSync prints a one-line verdict, or loud warnings, before
// data goes up. It never blocks: contents are encrypted either way.
func checkBucketBeforeSync(ctx context.Context, r Remote) {
	a, ok := r.(bucketAuditor)
	if !ok {
		fmt.Println("bucket: can't check privacy settings for this kind of remote")
		return
	}
	rep := a.auditBucket(ctx)
	for _, risk := range rep.Risks {
		fmt.Printf("⚠  %s\n", risk)
	}
	switch {
	case !rep.private():
		fmt.Printf("⚠  BUCKET %s MAY BE PUBLIC:\n", rep.Bucket)
		for _, p := range rep.Public {
			fmt.Printf("     - %s\n", p)
		}
		fmt.Println("   (contents are still encrypted; run `shasync remote check` for details)")
	case len(rep.Unknown) > 0:
		fmt.Printf("bucket %s: no public access found, but couldn't verify: %s\n", rep.Bucket, strings.Join(rep.Unknown, "; "))
	default:
		fmt.Printf("bucket %s: private ✓\n", rep.Bucket)
	}
}

// printBucketReport is `shasync remote check`.
func printBucketReport(rep *bucketReport) {
	fmt.Printf("bucket:   %s (%s)\n", rep.Bucket, firstNonEmpty(rep.Region, "region unknown"))
	if rep.Identity != "" {
		fmt.Printf("as:       %s\n", rep.Identity)
	}
	if rep.private() {
		fmt.Println("verdict:  private ✓")
	} else {
		fmt.Println("verdict:  MAY BE PUBLIC")
	}
	for _, p := range rep.Public {
		fmt.Printf("  ⚠ %s\n", p)
	}
	for _, risk := range rep.Risks {
		fmt.Printf("  ⚠ %s\n", risk)
	}
	for _, f := range rep.Facts {
		fmt.Printf("  · %s\n", f)
	}
	for _, u := range rep.Unknown {
		fmt.Printf("  ? couldn't check %s\n", u)
	}
}
