# Contributing to Particles

Thanks for your interest. This file covers the legal terms under which
contributions are accepted; see the README for everything else (build,
test, project layout).

Particles is licensed to the public under
**GPL-3.0-or-later** ([LICENSE](LICENSE)). This file does **not**
change that: the public license stays as it is.

What this file does is define the terms under which you grant rights
to the maintainer when you contribute, so the project can both stay
GPL-3.0-or-later for the public **and** be used by the maintainer in
their own (including proprietary) products.

## How to sign off your commits (DCO)

Every commit you submit must carry a `Signed-off-by:` trailer:

```
Signed-off-by: Jane Doe <jane@example.com>
```

You add it automatically with:

```
git commit --signoff
```

(or `git commit -s`). The trailer must match the name and email on
the commit's `Author` field.

Adding `Signed-off-by:` to a commit is your statement, as the
contributor, that the certifications in the next two sections are
true for that commit. No separate document to sign, no web form, no
account to create — the trailer is the agreement.

If you missed it on a commit, amend with `git commit --amend -s` (or
for a series, `git rebase --signoff <base>`) and force-push the branch
your PR is on. We can't merge without it.

## Section 1 — Developer Certificate of Origin 1.1

This section is the standard
[Developer Certificate of Origin 1.1](https://developercertificate.org/),
reproduced verbatim. By signing off a commit you certify the following:

> Developer Certificate of Origin
> Version 1.1
>
> Copyright (C) 2004, 2006 The Linux Foundation and its contributors.
>
> Everyone is permitted to copy and distribute verbatim copies of this
> license document, but changing it is not allowed.
>
>
> Developer's Certificate of Origin 1.1
>
> By making a contribution to this project, I certify that:
>
> (a) The contribution was created in whole or in part by me and I
>     have the right to submit it under the open source license
>     indicated in the file; or
>
> (b) The contribution is based upon previous work that, to the best
>     of my knowledge, is covered under an appropriate open source
>     license and I have the right under that license to submit that
>     work with modifications, whether created in whole or in part
>     by me, under the same open source license (unless I am
>     permitted to submit under a different license), as indicated
>     in the file; or
>
> (c) The contribution was provided directly to me by some other
>     person who certified (a), (b) or (c) and I have not modified
>     it.
>
> (d) I understand and agree that this project and the contribution
>     are public and that a record of the contribution (including all
>     personal information I submit with it, including my sign-off) is
>     maintained indefinitely and may be redistributed consistent with
>     this project or the open source license(s) involved.

## Section 2 — Inbound license grant

In addition to the DCO certifications above, by signing off a commit
you grant the project maintainer (the `partite-ai` organization, 
referred to here as "the Maintainer") the following rights in your
contribution. "Your contribution" means the code, documentation, and
any other material added or modified by the signed-off commit.

You retain copyright in your contribution. You do not transfer
ownership.

You grant to the Maintainer and to all downstream recipients of the
project a perpetual, worldwide, non-exclusive, royalty-free,
irrevocable copyright license to reproduce, prepare derivative works
of, publicly display, publicly perform, sublicense, and distribute
your contribution and such derivative works.

You grant to the Maintainer and to all downstream recipients of the
project a perpetual, worldwide, non-exclusive, royalty-free,
irrevocable (except as stated below) patent license to make, have
made, use, offer to sell, sell, import, and otherwise transfer your
contribution, where such license applies only to those patent claims
licensable by you that are necessarily infringed by your contribution
alone or by combination of your contribution with the project to
which it was submitted. If any entity institutes patent litigation
against you or any other entity (including a cross-claim or
counterclaim in a lawsuit) alleging that your contribution, or the
project to which you have contributed, constitutes direct or
contributory patent infringement, then any patent licenses granted to
that entity under this section for that contribution or project shall
terminate as of the date such litigation is filed.

You additionally grant to the Maintainer (and only to the Maintainer,
not to downstream recipients) the right to license your contribution
to third parties under **any license terms of the Maintainer's
choosing**, including proprietary, commercial, or other non-open-source
terms, with or without further consideration to you. This grant
exists so the Maintainer can use, sublicense, and distribute the
project — including your contribution — as part of the Maintainer's
own products and services without being limited to GPL-3.0-or-later.
The Maintainer's public release of the project remains under
GPL-3.0-or-later; this grant gives the Maintainer additional,
parallel rights, not a substitute for the public license.

You represent that you are legally entitled to grant the above
licenses. If your employer has rights to intellectual property you
create that includes your contribution, you represent that you have
received permission to make the contribution on behalf of that
employer, or that your employer has waived such rights for the
contribution.

You provide your contribution on an "AS IS" basis, without warranties
or conditions of any kind, either express or implied, including,
without limitation, any warranties or conditions of title,
non-infringement, merchantability, or fitness for a particular
purpose. You are not required to provide support for your contribution,
except to the extent you desire to provide support.

## Scope

This file governs contributions made on or after the date it is
committed to the repository. Contributions made before that date
remain governed by whatever terms applied at the time, which for
this project is the same arrangement codified retroactively here
(the project has had no third-party contributors prior to this
point).

If you have questions before contributing, open an issue or contact
the Maintainer.
