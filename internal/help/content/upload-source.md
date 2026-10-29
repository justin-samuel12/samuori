---
title: Upload your source
section: Building images
summary: Send your application's files to Samuori and see what it detects, without running anything.
roles: [developer, maintainer]
order: 10
---
Every image starts from a **source upload**: an immutable copy of your files. Builds always use an upload, never
your computer.

## Upload

1. Open your project from **Projects**, then **Upload source** (or start from **New image**).
2. **Choose files:** drag your project folder or a ZIP, TAR or TAR.GZ archive onto the drop area, or use **Choose
   folder** or **Choose ZIP or TAR**. The drop area shows the size and file limits your administrator set.
3. **Review what will be uploaded.** Samuori leaves out build output, dependency folders and files that usually hold
   credentials, and lists what it left out. Add more under **Also leave out (optional)**. If a file over a limit is
   reported, leave it out or ask an administrator about the limits.
4. **Upload.** Large uploads are sent in pieces; a piece that fails is retried a few times before the upload stops.

Browsers do not tell Samuori which files are executable. In a folder upload, files that start with `#!` or end in `.sh`
are made executable and nothing else is. If your app runs another kind of file directly, upload a `.tar.gz` or
`.zip` instead, which keeps the permissions you gave it.

Never upload real credentials. If your build needs a token or a certificate, use a secret key instead: see
[Secrets for your build](help:build-secrets).

## What Samuori finds

The **Uploaded source** page shows **What we found**: the language, framework, version and how it would be started,
each with how sure Samuori is. **Things to check** lists anything you should confirm. If the upload holds several
applications, choose one under **Which application do you want to build?**

Samuori reads your files only. It never runs your code, install scripts or Dockerfile to find this out.

## Delete an upload

If you uploaded the wrong files, open the upload (or the project's **Source uploads**) and choose **Delete**. Only the
person who uploaded it can delete it. Its files are removed at once; builds made from it stay in the history but
can no longer be built again from it. Images already built and published are not affected.

Next: [Create an image](help:new-image).
