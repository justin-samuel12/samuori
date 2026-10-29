---
title: Export and import configuration
section: Administration
summary: Copy this organization's settings to another installation, with a preview before anything changes.
roles: [administrator]
order: 245
---
**Export & import** saves this organization's configuration as one YAML file and applies such a file here.

The file holds policies, build methods and providers, recipe decisions and custom recipes, worker pools, registries,
destinations, notification channels and rules, sign-in settings and audit retention. It never holds secrets,
credentials, users, role bindings or projects.

## Export

**Download configuration** saves the file. Keep it under change control if you like; two exports of the same
configuration differ only in their date.

## Import

1. **Choose file.** Samuori compares the file with this organization and lists each item as create, update, unchanged
   or skip, with the reason. Nothing changes yet.
2. Read **Needs attention**: credentials and channels that hold a secret are not created, and some changes are left
   for the page where they are checked, such as a policy change that would block existing specs.
3. **Apply import** makes the changes, each checked and audited as if you made it by hand.
4. Add the credentials and channels it lists, then import the same file again to add what waited for them.

An import never deletes anything that is missing from the file, never moves an approval or status back, and refers to
projects as team/project; it never creates them.
