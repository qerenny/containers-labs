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
