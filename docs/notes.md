# Thoughts On Schedulers

## What a Good Scheduler Should Do
* Run jobs as DAGS. 
Dependency managment for the win. 
* Be able to run jobs that don't have a schedule. 
E.g. if I have a process that I run infrequently/irregularly, that has dependencies, I want to be able to trigger than in my framework. 
* Reproducibility. Code and config are all under version control. 
Log file contains import information like Git hashes, and Docker hashes. 
* Low entry barrier. 
Should be roughly on par with Cron. 
* Sophisticated schedule descriptions. E.g. "Run at 1700 on the last business day of the month."
* No operators. Execute code in containers. 
* Simple. Ideally we should be able to run the scheduler without needing a database, message queue, webserver, etc. 
* Rerun logic built into DAG definitions.
E.g. Always rerun if downstream job is rerun. 
* Scheduled outages. 
* First class command line support. 
* Parametrized. 
Make sure that certain pipelines are reusable, and can be rerun.
Also should support triggering a job in say history mode for example.  
* Batch size. 
If I want to rerun a pipeline for the last week say, the scheduler should automatically know to run it as seven jobs of one day each. 
* Statistics and metrics.
Which jobs are failing, or taking too long to run.
Which servers are causing issues. 
Which vendors are unreliable. 
* Supports but doesn't require orcestrators, specifically Kubernetes.
Will need a Helm chart.  

Also, if anything has to be defined in config file, it should be YAML. 

Does it make sense to consider things like database config, and secret managment?
E.g. if I defined a database in a config file

```yaml
database:
    dev:
        dialect:	postgres
        name: 		whisky
        host:     255.255.0.1
        port:  		5432
        user:     batman
        password: CatWoman69
```

Then somewhere in the job config I could do something like 

```python 
from caesium import config

database_config = config.database.dev
connection = get_database_connection(**database_config)
```

This would allow for uses to have a local config file for testing. 
Actually no, that's a terrible idea. 
In fact, we need to put some thought into how testing etc will work with the containers. 

Though I'm still not completly over the idea of using it for secret managment. 
I'm just not sure how yet. 

## Current Options

### Cron
* No dependencies between jobs.
* Cron is not Bash. 
* Version control isn't easy. 

### Airflow
* Currently the industry standard, for better or for worse. 

### Luigi
* Can get fucked because it's fucking garbage.

### Argo
* Requires Kubernetes.

### KubeFlow
* See Argo.
Literally runs on top of Argo. 

### Oozie
* Only works for Hadoop jobs. 

### Azkaban
* See Oozie.

### Pinball
* Not maintained.

It should also be noted that of these options AirFlow - and to a lesser extend Luigi - are the only ones with any real maturity or traction. 


