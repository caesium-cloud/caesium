# Dependencies

The age old question: to import or build yourself. Because Go has a relatively barebones standard library, the tendancy is to build everything yourself, from scratch, every time. Your own `Set` class or `ThreadPool` etc. In this document we will define our dependency strategy for the project, not only for the Go source, but also with respect to plugin support and adoption into the parent repository(ies).

## Go Modules

There are three main principals when it comes to code dependencies:

1) Is the code well written?
2) Will the code be maintained?
3) Will writing the code yourself have an intrinsic advantage?

If the answers to these questios is "Yes, Yes, No", then the obvious answer is to import the code and not waste the time implementing the same thing yourself. Each time we introduce a new dependency, these are the qeustions we need to ask ourselves as developers and if we don't answer "Yes, Yes, No" then we need to consider if this is a worthwhile dependency.

## Plugins

By definition, plugins are source code that is not part of the canonical main codebase, but introduces functionality that a good amount of users want. As a result, the threshold for plugins is different than for dependencies into the core source code. In general, plugins should be developed in third-party repos in accordance with the implementation pattern we provide to interact with Caesium. In the event that the plugin reaches critical mass, and many Caseium users are using the plugin and requesting features/support for it, then the plugin will be adopted in to the main codebase.