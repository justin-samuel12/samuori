---
title: Registries and destinations
section: Build supply
summary: Connect registries, store push credentials, and decide where projects may publish.
roles: [administrator]
order: 230
---
**Build supply > Registries & destinations** sets up where images go.

1. **Registries:** add each registry: a generic OCI registry or Amazon ECR (account ID and region). Add an ECR
   registry as kind **Amazon ECR**, not OCI, so its credentials can use IAM roles.
2. **Push credentials:** how Samuori signs in to push: a username and secret, or for ECR **This controller's IAM role**
   (the EC2 instance profile, nothing stored), **Assume a role** with an optional external ID, or an access key.
   Credentials stay on the Samuori server; builds never see them. **Test** checks them.
3. **Destinations:** a repository on a registry, opened to specific projects under **Projects that may publish
   here**. Options:
   - **Protected tags**, and **No tag may be moved to another image**;
   - **Publications need an approval**;
   - **Sign images** and **Attach signed build provenance (SLSA)**;
   - **Accept only promotions** of images published elsewhere;
   - **Accept restricted images**, for images that passed only through an output exception.
4. **Signing keys:** create the organization's signing key, held by Samuori or by an external KMS. **Retire** stops
   the active key from signing for good. Its public key stays listed, so images it signed still verify; keys are
   never deleted. Destinations that sign cannot publish until you create a new key.

**View or edit** and **Delete** work on registries and credentials. A registry's endpoint cannot change while its
credentials hold a stored password, token or key, so a secret is never sent somewhere new: delete those credentials
first. Anything still in use, by a destination for example, cannot be deleted; the message lists what uses it.

**Approved image sources** lets builds pull private base images through Samuori's mirror, limited to the teams you
choose; upstream credentials stay with the mirror. **Edit** changes a source's name, repositories, team and pull
credentials; the upstream and mirror are fixed. **Delete** is refused while a build of its team that has not finished
plans to pull from it: disable the source first, and delete it when those builds are done.
