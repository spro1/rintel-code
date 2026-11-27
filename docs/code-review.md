# Review a change

Before you push, or when you review someone else's branch: what does the change touch, what depends on it, and which
tests cover it.

[한국어](ko/code-review.md) · [All guides](README.md)

## What my uncommitted change affects

```
$ rintel impact
Compared with HEAD: 1 files changed, 1 symbols changed, 4 affected, 3 endpoints, 13 tests reach it.

Changed (1)
  Owner.addPet(Pet)  owner/Owner.java:100

Affected (4)
  PetController.initCreationForm(Owner,ModelMap)     1 step(s) away  owner/PetController.java:100
  PetController.processCreationForm(Owner,Pet,…)     1 step(s) away  owner/PetController.java:107
  PetController.updatePetDetails(Owner,Pet)          1 step(s) away  owner/PetController.java:186
  PetController.processUpdateForm(Owner,Pet,…)       2 step(s) away  owner/PetController.java:144

Endpoints (3)
  GET  /owners/{ownerId}/pets/new
  POST /owners/{ownerId}/pets/new
  POST /owners/{ownerId}/pets/{petId}/edit

Tests that reach the change (13)
  OwnerTests.addPetAddsPersistedPet()                1 step(s) away
  OwnerTests.addPetDoesNotAddDuplicatePet()          1 step(s) away
  ...
```

Read it from the top:

1. **Changed**: the methods and classes whose lines the diff touches.
2. **Affected**: production code that depends on them, nearest first.
3. **Endpoints**: the REST endpoints a request can take to reach the change. These are what to try by hand.
4. **Tests that reach the change**: the tests to run. If this list is empty and "Affected" is not, the change is
   untested; that is the main thing to say in a review.

## A whole branch

```sh
rintel impact --base origin/main            # everything this branch changed
rintel impact --base origin/main --depth 5  # follow dependents further (default 3, max 6)
```

## As a pull-request comment

```sh
rintel impact --base origin/main --format md > impact.md
gh pr comment --body-file impact.md
```

## Before deleting or renaming something

```sh
rintel callers PetValidator.validate --depth 2   # who calls it
rintel refs PetValidator                         # every mention: types, fields, injection, overrides
```

"No callers" means none that static analysis can see. Calls through reflection, templates or a framework rintel
does not model are invisible, so check `rintel stats` for the unresolved share before deleting public code.

## New problems only

On an existing code base `rintel check` may list hundreds of old findings. To see only what the branch touched:

```sh
rintel check --changed-since origin/main
```
