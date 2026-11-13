---
layout: home
title: Tack
titleTemplate: Move a database primary between sites
description: Tack moves the primary of a CloudNativePG database from one Kubernetes site to another, keeps Flux and Git in step, and records every step in a ledger.

hero:
    name: Tack
    text: Move a database primary between sites, on your command.
    tagline: A planned switchover, or a failover after a site is lost, for CloudNativePG deployed by Flux. Every step is planned first, recorded in a ledger, and resumable.
    actions:
        - theme: brand
          text: Get started
          link: /guide/getting-started
        - theme: alt
          text: How a run works
          link: /guide/concepts
        - theme: alt
          text: GitHub
          link: https://github.com/takodotid/tack

features:
    - title: You start every run
      details: Tack never fails over by itself. tack plan reads every site and Git and changes nothing; tack run asks you to type the workload's name.
    - title: Flux held narrowly
      details: Only the objects the run changes are held, with an annotation on the live objects. Flux applies them again once Git matches live.
    - title: The token carried across
      details: In a planned switchover, the old primary's demotion token is recorded and the new primary promotes with it. No write is lost.
    - title: A ledger for every step
      details: Each step is written before it starts and after it ends. A run that stops resumes from its ledger, never from memory.
    - title: Site loss, even with Git down
      details: Tack switches live, records the fork point, and commits once Git is back. Nothing is released before Git matches live.
    - title: Stops and asks
      details: Git changed, a token missing, a lost site back as primary, an object held by someone else. Each stops the run with a reason.
---
