# Лаба 4

# Часть 0 -- Делаем бесполезный batch

Для этой лабы нужен кто-то, кем потом можно будет пожертвовать. Поэтому делаем
`batch`: он держит память и бесконечно крутит CPU, но ничего полезного не
делает.

Управляем им двумя переменными:

```text
BATCH_MEMORY_MIB=64
BATCH_CPU_WORKERS=1
```

Собираем через multi-stage Dockerfile, запускаем от UID `65532`. Быстро
проверяем, что процесс стартует и нормально завершается по сигналу:

```bash
cd lab4/batch
go test ./...
BATCH_MEMORY_MIB=32 BATCH_CPU_WORKERS=1 go run .
```

# Часть 1 -- Как будем растаскивать API

У нас три реплики и всего две ноды. Жёсткая `podAntiAffinity` тут не подходит:
она запретит двум одинаковым Pod жить на одной ноде, и третья реплика останется
в `Pending` просто потому, что третьей ноды нет.

Поэтому выбираю `topologySpreadConstraints`:

```yaml
api:
  scheduling:
    mode: topologySpreadConstraints
```

С `maxSkew: 1` scheduler сможет разложить реплики как `2+1`. Перекос между
нодами не больше одного, но отдельную ноду под каждый Pod искать не надо. Так
дальше `Pending` из-за нехватки ресурсов не перепутается с запретом самой
топологии.

# Часть 2 -- Поднимаем тесный кластер

Создаём отдельный kind-кластер: один control-plane и один worker. Taint с
control-plane убираем, иначе из двух контейнеров-нод workload сможет жить
только на одном.

```yaml
nodes:
  - role: control-plane
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        nodeRegistration:
          taints: []
  - role: worker
```

С первого раза, конечно же, не завелось:

```text
failed to create fsnotify watcher: too many open files
failed to run Kubelet: ... unknown service runtime.v1.RuntimeService
```

Сначала можно подумать на `ulimit -n`, но он здесь вообще ни при чём. Кончился
`fs.inotify.max_user_instances`, поэтому containerd не смог создать watcher,
CRI не поднялся, а kubelet остался без runtime.

```bash
sudo sysctl -w fs.inotify.max_user_instances=1024
kind create cluster --name lab4 --config lab4/kind-config.yaml --retain
```

После этого обе ноды готовы и без taints:

```text
NAME                 STATUS   ROLES           VERSION
lab4-control-plane   Ready    control-plane   v1.34.11
lab4-worker          Ready    <none>          v1.34.11

NAME                 TAINTS
lab4-control-plane   <none>
lab4-worker          <none>
```

Теперь добавляем в чарт две реплики нашего бесполезного соседа:

```yaml
batch:
  replicaCount: 2
  BATCH_MEMORY_MIB: 128
  BATCH_CPU_WORKERS: 2
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 200m
      memory: 256Mi
```

Памяти в limit больше, чем размер буфера, потому что Go runtime и стеки тоже
не питаются воздухом. Requests и limits разные, значит `batch` получается
Burstable. Лимиты при этом на месте, Kyverno доволен.

Образы с Docker-хоста kind сам не увидит, поэтому отдельно загружаем `api`,
`worker` и `batch` в его containerd. Затем возвращаем в новый кластер Kyverno,
CloudNativePG и monitoring stack и ставим чарт:

```bash
helm upgrade --install shop lab4/helm/shop \
  --namespace shop \
  --create-namespace \
  --wait \
  --timeout 10m
```

```text
NAME          READY   AVAILABLE
shop-api      3/3     3
shop-batch    2/2     2
shop-worker   2/2     2

NAME      INSTANCES   READY   STATUS
shop-db   1           1       Cluster in healthy state
```

Получили две рабочие ноды и уже довольно много Pod. То, что нужно для
дальнейших издевательств над scheduler.

# Часть 3 -- Хватит угадывать requests

Запускаем нагрузку на `/order`, а потребление забираем из Prometheus через
KRR. Сначала захотелось взять короткое окно и поставить
`history_duration=0.25`, но эта версия KRR принимает минимум один час:

```text
history_duration
  ensure this value is greater than or equal to 1
```

Ждать целый час ради стенда не хочется. Оставляем часовое окно, но снижаем
минимум собранных точек со 100 до 20:

```bash
python krr.py simple \
  --namespace shop \
  --prometheus-url http://127.0.0.1:9090 \
  --history_duration 1 \
  --points_required 20 \
  --formatter json \
  --fileoutput /reports/krr-report.json
```

Теперь вместо `Not enough data` получаем числа:

| Workload | Было: requests | KRR | Поставили: requests | Поставили: limits |
|---|---|---|---|---|
| `api` | `100m / 128Mi` | `10m / 100Mi` | `10m / 100Mi` | `100m / 100Mi` |
| `worker` | `100m / 128Mi` | `10m / 100Mi` | `10m / 100Mi` | `100m / 100Mi` |
| `postgres` | `100m / 256Mi` | `15.1m / 117.1Mi` | `20m / 118Mi` | `500m / 118Mi` |

С PostgreSQL пришлось немного схитрить. KRR увидел Deployment'ы, но не увидел
CNPG `Cluster`, поэтому тот же расчёт делаем запросами в Prometheus: CPU —
99-й перцентиль, память — максимальный working set плюс 15%.

```text
CPU:    0.015086415461727336
Memory: 122800127.99999999 bytes
```

Округляем вверх до `20m` и `118Mi`. CPU limit KRR вообще советует убрать, но
наша политика требует и CPU, и memory limits. Поэтому requests берём из
замеров, а старые CPU limits пока оставляем. Иначе Kyverno просто не пропустит
новые Pod.

После `helm upgrade` база пересоздалась, а `api` и `worker` успели несколько
раз рестартануть:

```text
POD                          REASON   EXIT
shop-api-6b7f748ff8-k8n4l    Error    1
shop-worker-5f4cf8fb-p4dg4   Error    1

dial tcp 10.96.33.6:5432: connect: connection refused
```

Здесь приложения сами
завершились с кодом `1`, пока за Service никто не принимал соединения на
`5432`. PostgreSQL вернулся, kubelet перезапустил процессы, оба Deployment
докатились успешно.



# Часть 4 -- Растаскиваем API по нодам

Выбор в `values.yaml` сам по себе ничего не делал, потому что шаблон его ещё не
использовал. Добавляем constraint в Pod template API:

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: api
        app.kubernetes.io/part-of: shop
```

Selector считает только Pod'ы API, а topology domain здесь — hostname ноды.
Если новая реплика сделает перекос больше одного, `DoNotSchedule` не даст её
туда поставить.

После `helm upgrade` смотрим, куда всё разъехалось:

```text
NAME                        READY   STATUS    NODE
shop-api-7685546b7c-554bp   1/1     Running   lab4-control-plane
shop-api-7685546b7c-7cd9l   1/1     Running   lab4-worker
shop-api-7685546b7c-q9vmg   1/1     Running   lab4-control-plane

      2 lab4-control-plane
      1 lab4-worker
```

Получили ожидаемые `2+1`: разница равна единице, значит `maxSkew` соблюдён.
Теперь отказ одной ноды не заберёт сразу все реплики API — хотя бы одна часть
сервиса останется на второй.

# Часть 5 -- Назначаем первых на выход

Так, теперь говорим scheduler, что база и API нам всё-таки нужнее ночной
аналитики. В values заводим два класса:

```yaml
priorityClasses:
  critical:
    name: shop-critical
    value: 1000000
  batch:
    name: shop-batch
    value: -1000
```

Добавили два `PriorityClass`,
`priorityClassName` в Pod template API и batch, а для CNPG — прямо в
`Cluster.spec`. Для API ещё сделали свой PDB, базе его и так создаёт оператор.

Сначала, конечно, ищем несуществующий ресурс:

```bash
kubectl get pbd -n shop
error: the server doesn't have a resource type "pbd"

# pdb звучит всё-таки убедительнее
kubectl get pdb -n shop
NAME              MIN AVAILABLE   ALLOWED DISRUPTIONS
shop-db-primary   1               0
```


Прогоняем весь чарт через настоящий API, но пока ничего не
создаём:

```bash
kubectl apply --dry-run=server -f /tmp/lab4-rendered.yaml
```

```text
priorityclass.scheduling.k8s.io/shop-critical created (server dry run)
priorityclass.scheduling.k8s.io/shop-batch created (server dry run)
poddisruptionbudget.policy/shop-api-pdb created (server dry run)
deployment.apps/shop-api created (server dry run)
deployment.apps/shop-batch created (server dry run)
cluster.postgresql.cnpg.io/shop-db created (server dry run)
```

Схема живая, можно ставить. API и PostgreSQL отправляем в `shop-critical`,
batch — в `shop-batch`. Ещё выравниваем requests и limits,
чтобы получить `Guaranteed`. У batch они разные, он остаётся
`Burstable`.

На радостях ставим API `10m`, базе `20m`, потому что такие requests показал
KRR:

```bash
helm upgrade shop lab4/helm/shop \
  --namespace shop \
  --wait \
  --timeout 10m
```

И получаем десятиминутный Helm timeout и `CrashLoopBackOff`:

```text
UPGRADE FAILED:
Deployment/shop/shop-api not ready: Updated: 1/3
Deployment/shop/shop-worker not ready: Available: 0/2
Cluster/shop/shop-db not ready: Cluster Is Not Ready
context deadline exceeded
```

Смотрим Pod'ы и Events:

```text
shop-api      0/3   CrashLoopBackOff
shop-worker   0/2   CrashLoopBackOff
shop-db       Primary instance is being restarted without a switchover

Startup probe failed: Client.Timeout exceeded while awaiting headers
Readiness probe failed: context deadline exceeded
Liveness probe failed: HTTP probe failed with statuscode: 503
```

Никакого `OOMKilled` нет. Тут мы сами задушили базу CPU limit'ом. Её probes не
успевали ответить, а `/health` у API и worker ходит через `db.Ping()`. База
тормозит — health даёт `503`,  liveness начинает бодро перезапускать вообще
всё приложение.

Requests от KRR оказались нормальной оценкой спокойного потребления, но плохим
 потолком на старте. Поднимаем API и PostgreSQL до `100m`, причём сразу
и request, и limit, чтобы не потерять `Guaranteed`.




```bash
helm upgrade shop lab4/helm/shop \
  --namespace shop \
  --wait \
  --atomic \
  --timeout 15m
```

Седьмая ревизия наконец доезжает:

```text
STATUS: deployed
REVISION: 7
DESCRIPTION: Upgrade complete
```

Что реально получил каждый
Pod:

```bash
kubectl get pods -n shop \
  -o custom-columns='NAME:.metadata.name,QOS:.status.qosClass,PRIORITY:.spec.priority,CLASS:.spec.priorityClassName'
```

```text
NAME                         QOS          PRIORITY   CLASS
shop-api-77fdc89-kksxd       Guaranteed   1000000    shop-critical
shop-api-77fdc89-r2rzv       Guaranteed   1000000    shop-critical
shop-api-77fdc89-xxnmm       Guaranteed   1000000    shop-critical
shop-batch-7bb6b7db8-hzqch   Burstable    -1000      shop-batch
shop-batch-7bb6b7db8-wb8gw   Burstable    -1000      shop-batch
shop-db-1                    Guaranteed   1000000    shop-critical
shop-worker-5f4cf8fb-p4dg4   Burstable    0          <none>
shop-worker-5f4cf8fb-p9nmt   Burstable    0          <none>
```

Получилось как и хотели: О`Guaranteed` и самый приоритетный,
batch ниже даже обычного worker. Теперь отдельно смотрим PDB:

```bash
kubectl get pdb -n shop
```

```text
NAME              MIN AVAILABLE   ALLOWED DISRUPTIONS
shop-api-pdb      2               1
shop-db-primary   1               0
```

То есть из трёх API можно добровольно выселить один. Единственную базу — не могём. Сам priority пока никого не выгоняет, это случится только когда докатится
более важный Pod и ему реально не хватит места.

# Часть 6 -- Теперь места действительно не хватает

С исходными `100m` у batch пришлось бы наплодить десятки Pod'ов: на каждой
ноде доступно по восемь CPU. Для эксперимента сделаю одну задачу 
тяжелее:

```yaml
batch:
  replicaCount: 8
  resources:
    requests:
      cpu: "2"
      memory: 256Mi
    limits:
      cpu: "3"
      memory: 512Mi
```

Главное для этого опыта — request достаточно большой,
чтобы упереться в ёмкость нод небольшим числом реплик. После `helm upgrade`
из восьми batch разместились шесть, ещё два остались ждать:

```text
NAME                          STATUS    NODE
shop-batch-54b7b6cc96-2r6jg  Running   lab4-worker
shop-batch-54b7b6cc96-6k2nf  Running   lab4-control-plane
shop-batch-54b7b6cc96-8mdzw  Running   lab4-control-plane
shop-batch-54b7b6cc96-8wgfl  Running   lab4-worker
shop-batch-54b7b6cc96-qsr45  Running   lab4-control-plane
shop-batch-54b7b6cc96-tjjdp  Running   lab4-worker
shop-batch-54b7b6cc96-44crr  Pending   <none>
shop-batch-54b7b6cc96-q4qtr  Pending   <none>
```

Scheduler объяснил это так:

```text
0/2 nodes are available: 2 Insufficient cpu.
preemption: 0/2 nodes are available: 2 No preemption victims found for incoming pod.
```

`Insufficient cpu` здесь означает нехватку не фактических тактов, а свободного
бюджета requests. А вытеснить кого-нибудь новый
batch не может: его priority равен `-1000`, остальные batch имеют тот же
priority, а API, PostgreSQL и обычные worker стоят выше.

Чтобы увидеть уже настоящее preemption, временно поднимаем API с трёх до 25
реплик. Когда очередной `shop-critical` уже не помещается,
scheduler находит ему жертву среди `shop-batch`:

```text
Preempted  pod/shop-batch-54b7b6cc96-qsr45
Preempted by pod 67d0fba9-736e-41f6-a326-89954db0e8bc
on node lab4-control-plane
```

UID принадлежал `shop-api-77fdc89-c47zf`. Все
25 API стали `Running`, а Deployment batch тут же создал замену
`shop-batch-54b7b6cc96-lzm7m`. Замена
осталась `Pending`: контроллер держит  желаемые восемь реплик, но
разместить их всё равно решает scheduler.

# Часть 7 -- Теперь кончается уже настоящая память

Перед опытом уменьшаем batch до шести реплик, чтобы старые `Pending` из
предыдущей части не путались с eviction. Заодно смотрим настройки kubelet и
обнаруживаем сюрприз:


```json
{
  "evictionHard": {
    "imagefs.available": "0%",
    "nodefs.available": "0%",
    "nodefs.inodesFree": "0%"
  }
}
```

Порога `memory.available` вообще нет. Если просто начать есть память в таком
виде, раньше kubelet может прийти kernel OOM killer, а это уже другое.
Добавляем в `KubeletConfiguration` threshold `12Gi` и
перезапускаем kubelet на обеих kind-нодах. 

```bash
kubectl wait nodes --all \
  --for=condition=Ready \
  --timeout=120s
```

```text
lab4-control-plane
{
  "imagefs.available": "0%",
  "memory.available": "12Gi",
  "nodefs.available": "0%",
  "nodefs.inodesFree": "0%"
}
lab4-worker
{
  "imagefs.available": "0%",
  "memory.available": "12Gi",
  "nodefs.available": "0%",
  "nodefs.inodesFree": "0%"
}
```

Сам batch временно разгоняем так:

```yaml
batch:
  replicaCount: 6
  BATCH_MEMORY_MIB: 768
  resources:
    requests:
      cpu: "2"
      memory: 256Mi
    limits:
      cpu: "3"
      memory: 1Gi
```

Потребление выше request, но всё ещё ниже собственного limit. После
`helm upgrade` `MemoryPressure` на control-plane меняется с `False` на `True`,
и kubelet выбирает две жертвы:

```text
NODE                 MEMORY_PRESSURE
lab4-control-plane   False
lab4-worker          False
lab4-control-plane   True
lab4-control-plane   True
```

```text
Warning  Evicted  pod/shop-batch-58d9b4ddd9-hlwgd
The node was low on resource: memory. Threshold quantity: 12Gi,
available: 12462136Ki. Container batch was using 790340Ki,
request is 256Mi, has larger consumption of memory.

Warning  Evicted  pod/shop-batch-58d9b4ddd9-vvt2t
The node was low on resource: memory. Threshold quantity: 12Gi,
available: 12464028Ki. Container batch was using 790556Ki,
request is 256Mi, has larger consumption of memory.
```

```bash
kubectl get pods -n shop \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,QOS:.status.qosClass,PRIORITY:.spec.priority,NODE:.spec.nodeName'
```

```text
NAME                          PHASE       QOS          PRIORITY   NODE
shop-api-77fdc89-42tsp        Running     Guaranteed   1000000    lab4-worker
shop-api-77fdc89-p5g4t        Running     Guaranteed   1000000    lab4-worker
shop-api-77fdc89-xxnmm        Running     Guaranteed   1000000    lab4-worker
shop-batch-58d9b4ddd9-hlwgd   Succeeded   Burstable    -1000      lab4-control-plane
shop-batch-58d9b4ddd9-vvt2t   Succeeded   Burstable    -1000      lab4-control-plane
shop-db-1                     Running     Guaranteed   1000000    lab4-worker
```

То есть первым ушёл именно `Burstable` batch с priority `-1000`, который к
тому же заметно вышел за свой memory request. API и PostgreSQL с
`Guaranteed` и priority `1000000` всё это время остались `Running`.
Deployment, конечно, сразу начал создавать batch заново. Один из новых Pod поэтому успел оказаться в
`Pending`, пока нода оставалась под давлением.

В списке Pod вытесненные процессы выглядели как `Succeeded/Completed`, а не
`Failed/Evicted`. Это наш batch аккуратно поймал `SIGTERM` и завершился с
нулевым кодом,
причину доказывает событие kubelet с `reason=Evicted` и сообщением `The node was low on resource:
memory`.

Вот это отличие от `OOMKilled`. При OOM по собственному limit решение
принимает cgroup, в `lastState.terminated.reason` появляется
`OOMKilled`, а контейнер обычно перезапускается внутри того же Pod. Здесь
лимит `1Gi` не был достигнут: kubelet увидел давление на всю ноду и удалил Pod
целиком, после чего Deployment создал новый объект.


