# Primitives

Caesium's primary function is a job scheduler and executor. As such, the design and implementation of this process will revolve around a set of primitives which aid in defining the lifcycle of a job. Below is an outline of how the primitives will be structured and implemented. Note that this is not a detailed implementation proposal, but rather meant merely as a holistic, pre-implementation guide for how Caesium's internals will be structured.

## `Job`

A `Job` is collection of related tasks. All the tasks in a single Job will depend on the same `Trigger`(s), or the completion of the other tasks in the `Job`. A `Job` is analogous to a `DAG` in Airflow, and is also defined as a directed acyclic graph. A `Job` can contain other primitive types in addition to `Task`s, as described below.

### `Trigger`

`Job`s are initiated by one or many `Trigger`s. A `Trigger` may be defined as a simple cron, or import a plugin that will listen, or consume from a remote source. Examples of such remote sources would be an AWS SQS queue, a Kafka topic, or a simple webhook. Caesium will, in general, rely heavily on [Go plugins](https://golang.org/pkg/plugin/), and this will be another example of where they will be leveraged.

### `Task`

A `Job` is primarily comprised of `Task`s in the same way that Airflow `DAG`s are comprised of `Task`s. Once a `Job` is triggered, Caesium will traverse the acyclic graph and execute each of the tasks. A task should not be designed to be long-running, and the underlying process' runtime should only be limited by compute, network or IO constraints (i.e. don't have arbitary sleeps in your task code).

#### `Container`

Caesium `Task`s run in `Container`s. A `Container`, while can be, is not necessarily the traditional definition of a container like in Docker, rkt, or LXD. Just like how `Trigger`s will be imported and run using Go plugins, so will `Container`s. A Caesium `Container` is defined as an isolated runtime environment for a `Job`'s `Task`s to run in with dedicated compute, network and IO resources. Concrete examples of `Container`s include Docker containers, Kubernetes pods, a Slurm job with dedicated resources allocated, or an EC2 on-demand or spot instance.

### `Callback`

`Callback`s are essentially operations that occur once a `Job` finishes, successfully or otherwise. The `Callback` will be passed the execution status of the `Job` to which it is attached, as well as the output of the last `Task` that was executed. There are several built-in forms of `Callback`s and custom ones can also be developed and linked in via the Go plugin interface (much in the same way CoreDNS implements custom plugins).

Note that, like `Task`s, a `Job`'s `Callback`s will run in `Container`s.

#### `Notification`

A `Notification` is a `Callback` that does what its name implies - notifies interested party that a `Job` has finished, and the state of its execution. A `Notification` will be delivered exactly once and requires no action or acknowledgement. Default mediums used to notify will include email and text message. Other third-party messaging products such as Slack can be used via a plugin.

#### `Alert`

An `Alert` is a `Callback` which is similar to a `Notification`, but which *does* require an action/acknowledgement to resolve. An `Alert` can be ack-ed directly via the Caesium API, CLI, or the web dashboard (once it is released). Like `Notification`s, default mediums used to alert will include email and text message. Other third-party messaging products such as Slack can be used via a plugin.

#### `Webhook`

A `Webhook`, otherwise known as a user-defined HTTP callback, is a `Callback` that makes a simple `POST` request to a specified endpoint. The endpoint, as well as the headers and payload will be parameters to the `Webhook` which will accompany the `Webhook`'s default content.

#### `Custom`

`Custom` `Callback`s are user defined operations that accept the same parameters that other forms of `Callback`s do, but have a user-defined `Container` and arguments that are passed to it. This allows users to completely specify their own `Callback` behaviors to suit their individual use-cases.