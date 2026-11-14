package organizations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/layout"
	"github.com/giantswarm/gitops-commit/sopsenc"

	"github.com/giantswarm/agent-manager/internal/agents"
)

func fileDigest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

func credentialGitObject(raw []byte) (*unstructured.Unstructured, error) {
	decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var object map[string]any
	if decoder.Decode(&object) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("%w: credential files must contain one Kubernetes object", agents.ErrConflict)
	}
	parsed := unstructuredOf(object)
	if parsed.GetKind() == "" || parsed.IsList() || parsed.GetName() == "" {
		return nil, fmt.Errorf("%w: a credential consumer identity cannot be inspected", agents.ErrConflict)
	}
	return parsed, nil
}

func (c *orgCommit) credentialSnapshot(ctx context.Context, operation *credentialOperation) (map[string]string, error) {
	directory, err := c.credentialDirectory(operation.Namespace)
	if err != nil {
		return nil, err
	}
	paths := []string{layout.SOPSConfigFile, directory.Path() + "/" + layout.KustomizationFile, path.Dir(directory.Path()) + "/" + layout.KustomizationFile}
	for _, planned := range operation.Writes {
		paths = append(paths, contentFile(directory, planned.Kind, planned.Name))
	}
	for _, credential := range operation.Credentials {
		paths = append(paths, contentFile(directory, kindSecret, credential.Secret))
	}
	files, err := c.remote.ListFiles(ctx, c.cfg.Objects.Repository, c.cfg.Objects.Branch, directory.Path())
	if err != nil {
		return nil, commitError(c.gh, err)
	}
	paths = append(paths, files...)
	snapshot := map[string]string{}
	scanned := map[string]bool{}
	var gitConsumers []CredentialReference
	for _, file := range paths {
		raw, err := c.remote.ReadFile(ctx, c.cfg.Objects.Repository, c.cfg.Objects.Branch, file)
		if errors.Is(err, commit.ErrFileNotFound) {
			snapshot[file] = "absent"
			continue
		}
		if err != nil {
			return nil, commitError(c.gh, err)
		}
		snapshot[file] = fileDigest(raw)
		for _, credential := range operation.Credentials {
			if file == contentFile(directory, kindSecret, credential.Secret) {
				object, err := credentialGitObject(raw)
				if err != nil || object.GetKind() != kindSecret {
					return nil, fmt.Errorf("%w: the existing encrypted Secret identity cannot be inspected", agents.ErrConflict)
				}
				var stored struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
					Data       map[string]any `json:"data"`
					StringData map[string]any `json:"stringData"`
				}
				if yaml.Unmarshal(raw, &stored) != nil {
					return nil, fmt.Errorf("%w: the existing encrypted Secret cannot be inspected", agents.ErrConflict)
				}
				if stored.Metadata.Name != credential.Secret {
					return nil, fmt.Errorf("%w: the encrypted Secret identity cannot be verified without decryption", agents.ErrConflict)
				}
				for _, data := range []map[string]any{stored.Data, stored.StringData} {
					for key := range data {
						if key != credential.Key {
							return nil, fmt.Errorf("%w: the encrypted Secret contains other credential keys", agents.ErrConflict)
						}
					}
				}
			}
		}
		if (strings.HasSuffix(file, ".yaml") || strings.HasSuffix(file, ".yml") || strings.HasSuffix(file, ".json")) && file != layout.SOPSConfigFile && !sopsenc.IsSecretFile(file) && !strings.HasSuffix(file, "/"+layout.KustomizationFile) {
			consumer, err := credentialGitObject(raw)
			if err != nil {
				return nil, err
			}
			for _, credential := range operation.Credentials {
				for _, reference := range referencesOf(consumer.GetKind(), consumer, credential.Secret) {
					if !writesFor(operation.Result, reference) {
						return nil, fmt.Errorf("%w: another Git consumer references the credential Secret", agents.ErrConflict)
					}
					if operation.Result.Kind == KindCredential && reference.Key != credential.Key {
						return nil, otherKey(reference, credential.Secret, credential.Key)
					}
					if operation.Result.Kind == KindCredential && !scanned[file] {
						gitConsumers = append(gitConsumers, reference)
					}
				}
			}
			scanned[file] = true
		}
	}
	// What git applies after the merge receives the value: it must be what
	// the person confirmed, the live consumers, until Flux catches up.
	if operation.Result.Kind == KindCredential && !sameConsumers(gitConsumers, operation.Consumers) {
		return nil, consumersDiffer{fmt.Errorf("%w: the objects in %s that use Secret %s, or where they send its value, differ from the live ones the person confirms: wait for Flux to apply git, then prepare it again", agents.ErrConflict, c.cfg.Objects.Repository, operation.Credentials[0].Secret)}
	}
	for _, planned := range operation.Writes {
		file := contentFile(directory, planned.Kind, planned.Name)
		if planned.Action == opCreate && snapshot[file] != "absent" {
			return nil, nameTaken{fmt.Errorf("%w: %s is in %s already and not applied yet: wait for the organization's Kustomization to apply it, then change it", agents.ErrConflict, file, c.cfg.Objects.Repository)}
		}
	}
	snapshot["directory-entries"] = fileDigest([]byte(strings.Join(files, "\n")))
	return snapshot, nil
}

func (s *Service) commitCredentialOperation(ctx context.Context, operation *credentialOperation, writer *contentWriter, writes []write) (*CommitResult, error) {
	current, err := writer.commit.credentialSnapshot(ctx, operation)
	if err != nil {
		return nil, err
	}
	if !maps.Equal(current, operation.GitFiles) {
		return nil, fmt.Errorf("%w: Git files or encryption policy changed; prepare the credential operation again", agents.ErrConflict)
	}
	change := fileChange{dir: writer.dir, write: map[string][]byte{}}
	isSecret, err := writer.commit.secretFiles(ctx)
	if err != nil {
		return nil, err
	}
	for _, planned := range writes {
		file := contentFile(writer.dir, planned.kind, planned.name)
		if planned.kind != kindSecret {
			if err := refusePlainAsSecret(isSecret, file, planned.kind, planned.name); err != nil {
				return nil, err
			}
		}
		change.write[file] = manifest(unstructuredOf(planned.obj), false)
		if planned.kind == kindSecret && operation.GitFiles[file] != "absent" {
			index := slices.IndexFunc(operation.Credentials, func(credential operationCredential) bool { return credential.Secret == planned.name })
			if index < 0 || !operation.Credentials[index].Replace {
				return nil, fmt.Errorf("%w: replacing an existing encrypted Secret requires explicit replacement", agents.ErrConflict)
			}
			change.replace = append(change.replace, file)
		}
	}
	var options []layout.Option
	if len(change.replace) > 0 {
		options = append(options, layout.ReplaceSecrets(change.replace...))
	}
	plan, err := layout.Build(ctx, writer.commit.remote, change.dir, change.write, nil, options...)
	if err != nil {
		return nil, commitError(writer.commit.gh, err)
	}
	landed := plan.Change()
	for _, credential := range operation.Credentials {
		file := contentFile(writer.dir, kindSecret, credential.Secret)
		var encrypted struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
			SOPS map[string]any    `json:"sops"`
		}
		if yaml.Unmarshal(landed.Files[file], &encrypted) != nil || encrypted.Kind != kindSecret || encrypted.Metadata.Name != credential.Secret || len(encrypted.SOPS) == 0 || !strings.HasPrefix(encrypted.Data[credential.Key], "ENC[AES256_GCM,") {
			return nil, fmt.Errorf("%w: the encryption policy does not encrypt every submitted credential field", agents.ErrInvalid)
		}
	}
	operation.WrittenFiles = map[string]string{}
	result := &CommitResult{Repository: writer.commit.cfg.Objects.Repository.String(), Base: writer.commit.cfg.Objects.Branch, Branch: "agent-manager/credential-" + operation.ElicitationID, Author: writer.commit.gh.Login, Review: reviewNote, Files: []agents.CommitFile{}}
	for _, file := range plan.Files {
		result.Files = append(result.Files, agents.CommitFile{Path: file.Path, Action: string(file.Action)})
		if raw, ok := landed.Files[file.Path]; ok {
			operation.WrittenFiles[file.Path] = fileDigest(raw)
		}
	}
	if len(operation.WrittenFiles) == 0 {
		return nil, errors.New("credential operation produced no encrypted change")
	}
	operation.Result.Commit = result
	if err := s.cfg.CredentialOperations.put(ctx, operation); err != nil {
		return nil, err
	}
	err = writer.commit.remote.CreateBranch(ctx, writer.commit.cfg.Objects.Repository, result.Branch, result.Base)
	if err != nil {
		return nil, commitError(writer.commit.gh, err)
	}
	branchConfiguration := *writer.commit.cfg
	branchConfiguration.Objects.Branch = result.Branch
	branchReader := *writer.commit
	branchReader.cfg = &branchConfiguration
	branchSnapshot, err := branchReader.credentialSnapshot(ctx, operation)
	if err != nil {
		return nil, err
	}
	if !maps.Equal(branchSnapshot, operation.GitFiles) {
		return nil, fmt.Errorf("%w: the Git base changed before the credential write", agents.ErrConflict)
	}
	err = writer.commit.remote.Commit(ctx, writer.commit.cfg.Objects.Repository, result.Branch, "chore(organizations): save credential operation", landed.Files)
	if err != nil {
		return nil, commitError(writer.commit.gh, err)
	}
	pullRequest, err := writer.commit.remote.OpenPullRequest(ctx, writer.commit.cfg.Objects.Repository, result.Branch, result.Base, "chore(organizations): save credential operation", "Updates the confirmed organization resources and their encrypted credentials. Operation: "+operation.ElicitationID+".\n\nThe platform team reviews the change through CODEOWNERS.")
	if err != nil {
		return nil, commitError(writer.commit.gh, err)
	}
	result.PullRequest, result.Number = pullRequest.URL, pullRequest.Number
	return result, nil
}

func (s *Service) recoverCredentialCommit(ctx context.Context, operation *credentialOperation) (*ResourceResult, error) {
	if operation.Result.Commit == nil || len(operation.WrittenFiles) == 0 {
		return nil, fmt.Errorf("%w: credential save has an uncertain outcome; inspect the operation before retrying", agents.ErrConflict)
	}
	writer, err := s.contentWriter(ctx, operation.Namespace, agents.ModeCommit)
	if err != nil {
		return nil, err
	}
	result := operation.Result.Commit
	for file, digest := range operation.WrittenFiles {
		contents, err := writer.commit.remote.ReadFile(ctx, writer.commit.cfg.Objects.Repository, result.Branch, file)
		if err != nil || fileDigest(contents) != digest {
			return nil, fmt.Errorf("%w: credential commit has an uncertain outcome; inspect branch %s before retrying", agents.ErrConflict, result.Branch)
		}
	}
	pullRequest, err := writer.commit.remote.OpenPullRequest(ctx, writer.commit.cfg.Objects.Repository, result.Branch, result.Base, "chore(organizations): save credential operation", "Completes credential operation "+operation.ElicitationID+". The platform team reviews the change through CODEOWNERS.")
	if err != nil {
		return nil, commitError(writer.commit.gh, err)
	}
	result.PullRequest, result.Number = pullRequest.URL, pullRequest.Number
	completeCredentialOperation(operation)
	if err := s.cfg.CredentialOperations.put(ctx, operation); err != nil {
		return nil, err
	}
	return &operation.Result, nil
}
