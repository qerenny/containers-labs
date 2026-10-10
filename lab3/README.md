# Лаба 3

# Часть 0 -- наши подопытные

Для лабы сделал два простых сервиса: `api` принимает заказ и кладёт его в
PostgreSQL, а `worker` забирает новые заказы и помечает их обработанными.

У `api` есть:

```text
POST /order
GET  /orders
GET  /health
GET  /metrics
```

У воркера тоже есть healthcheck и метрики. Ещё оба сервиса умеют специально
ломать healthcheck через `HEALTH_FAIL=true` -- потом посмотрим, что с этим
сделает Kubernetes.

Образы собираются через multi-stage, а внутри контейнера процесс запускается
не от root, а от UID `65532`.

# Часть 1 -- поставим охранника

В этой части нужен кто-то, кто будет смотреть на манифест до его создания.
Этим кем-то будет Kyverno.

```bash
helm upgrade --install kyverno kyverno/kyverno \
  --namespace kyverno \
  --create-namespace \
  --version 3.9.1 \
  --wait \
  --timeout 10m
```

```text
NAME: kyverno
NAMESPACE: kyverno
STATUS: deployed
Chart version: 3.9.1
Kyverno version: v1.19.1
```

Смотрим, не умерло ли что-нибудь по дороге:

```bash
kubectl get pods -n kyverno
```

```text
kyverno-admission-controller-...    1/1   Running
kyverno-background-controller-...   1/1   Running
kyverno-cleanup-controller-...      1/1   Running
kyverno-reports-controller-...      1/1   Running
```

Одна реплика admission controller -- не HA, о чём Helm честно предупредил.
Для локального kind переживём.

Ещё Helm сказал, что старые `ClusterPolicy` уже deprecated. Поэтому политики
здесь сделаны через:

```yaml
apiVersion: policies.kyverno.io/v1
kind: ValidatingPolicy
```

Для экспериментов создал отдельный namespace, чтобы не воевать со всем
кластером сразу:

```bash
kubectl apply -f policy-tests/namespace.yaml
```

## Первая политика, первые пострадавшие

Сначала потребовал у каждого контейнера `requests` и `limits` для CPU и
памяти.

```yaml
object.spec.containers.all(container,
  has(container.resources.requests.cpu) &&
  has(container.resources.requests.memory) &&
  has(container.resources.limits.cpu) &&
  has(container.resources.limits.memory)
)
```

`all` нужен, чтобы один приличный контейнер не прикрывал второй без
ограничений.

`requests` использует scheduler, когда решает, на какую ноду поставить Pod.
При упоре в CPU limit получаем throttling, при упоре в memory limit можно
получить `OOMKilled`.

После включения background scan Kyverno радостно пошёл проверять уже
существующие системные workloadы. 

## А проверяли Pod, почему тогда Deployment тоже не пускают

Сами правила смотрят на `Pod`, а тесты сделаны через `Deployment`. Тут
срабатывает autogen:

```yaml
autogen:
  podControllers:
    controllers:
      - deployments
      - statefulsets
      - daemonsets
      - cronjobs
      - jobs
```

Kyverno берёт Pod-правило и применяет его к `spec.template` контроллера.
Поэтому Deployment не сможет сначала пройти admission, а потом наплодить
неправильные Pod'ы.

Заодно правило работает для StatefulSet, DaemonSet, Job и CronJob. У них
разное поведение, но источник Podов тот же -- шаблон внутри контроллера.

## Остальной забор

Дальше добавил ещё четыре правила.

Разрешил образы только из наших registry:

```text
registry.k8s.io/
docker.io/qerenny/
```

Слеш на конце важен, иначе под префикс мог бы замаскироваться адрес вроде
`registry.k8s.io.evil.com`..

Потребовал у Pod два label:

```text
app.kubernetes.io/name
app.kubernetes.io/part-of
```

Первый называет компонент, второй связывает его с приложением целиком.
У Deployment autogen ищет их уже в `spec.template.metadata.labels`.

Запретил `securityContext.privileged: true`. Если `securityContext` или само
поле `privileged` отсутствует, это нормально: по умолчанию оно равно `false`.

Ну и запретил явный `:latest`:

```yaml
!container.image.endsWith(':latest')
```

С таким тегом два одинаковых rollout могут скачать разные слои. Правда,
текущая проверка запрещает только явно написанный `:latest`. Образ вообще без
тега она пропустит, хотя runtime тоже будет считать его latest. Оставлю это
как известное ограничение политики, а не буду делать вид, что его нет.

В итоге получилось пять политик:

```text
require-resource-limits
require-trusted-registry
require-standard-labels
disallow-privileged-containers
disallow-latest-tag
```

## Попробуем пролезть

Под каждое правило сделал по одному плохому Deployment:

```text
missing-resource-limits.yaml
untrusted-registry.yaml
missing-required-labels.yaml
privileged-container.yaml
latest-image-tag.yaml
```

Проверяю через server-side dry run. Так запрос реально доходит до API server
и admission webhook, но объект не записывается в etcd.

```bash
kubectl apply \
  --dry-run=server \
  -f policy-tests/missing-resource-limits.yaml
```

```text
Error from server: admission webhook "vpol.validate.kyverno.svc-fail"
denied the request: Policy require-resource-limits failed:
Every container must define CPU and memory requests and limits.
```

Первого поймали. Остальные тоже далеко не ушли:

```text
Policy require-trusted-registry failed:
Images must come from registry.k8s.io or docker.io/qerenny.

Policy require-standard-labels failed:
Pods must define app.kubernetes.io/name and app.kubernetes.io/part-of labels.

Policy disallow-privileged-containers failed:
Privileged containers are not allowed.

Policy disallow-latest-tag failed:
Images must not use the mutable latest tag.
```

Но политика, которая запрещает вообще всё, тоже отлично прошла бы такие
тесты. Поэтому нужен нормальный Deployment, который должен выжить:

```bash
kubectl apply \
  --dry-run=server \
  -f policy-tests/compliant-workload.yaml
```

```text
deployment.apps/compliant-workload created (server dry run)
```

Вот теперь порядок: пять плохих манифестов остановились на admission, а
`compliant-workload` прошёл все политики сразу.
