# Lambda runtimes

Stratus runs Lambda functions in Docker on AWS's own Lambda base images
(`public.ecr.aws/lambda/*`). Every base image ships the Runtime Interface
Emulator (RIE), so the code that loads your handler is the same code Lambda
runs. There is no Stratus-specific runtime shim per language.

## Supported runtimes

| Runtime | Image |
| --- | --- |
| `python3.9` – `python3.13` | `python:3.9` – `python:3.13` |
| `nodejs18.x`, `nodejs20.x`, `nodejs22.x` | `nodejs:18`, `nodejs:20`, `nodejs:22` |
| `java8.al2`, `java11`, `java17`, `java21` | `java:8.al2`, `java:11`, `java:17`, `java:21` |
| `ruby3.2`, `ruby3.3` | `ruby:3.2`, `ruby:3.3` |
| `dotnet8` | `dotnet:8` |
| `provided.al2`, `provided.al2023` (Go, Rust, any custom runtime) | `provided:al2`, `provided:al2023` |

Images are relative to `public.ecr.aws/lambda`. Container-image functions
(`PackageType: Image`) are supported too, see below.

## Images are never pulled

Like the RDS and ElastiCache engines, Stratus never pulls images, so runs stay
deterministic and work offline. Invoking a function whose image is missing
fails with a `ServiceException` that names the image. Pre-pull the runtimes you
use in CI:

```yaml
- run: |
    docker pull public.ecr.aws/lambda/java:21
    docker pull public.ecr.aws/lambda/provided:al2023
```

## Private registries and air-gapped networks

| Variable | Effect |
| --- | --- |
| `STRATUS_LAMBDA_IMAGE_REPOSITORY` | Replaces `public.ecr.aws/lambda` for every runtime, e.g. `registry.corp.example/aws-lambda`. |
| `STRATUS_LAMBDA_IMAGE_<RUNTIME>` | Overrides one runtime's image. The runtime is upper-cased with other characters as `_`: `STRATUS_LAMBDA_IMAGE_JAVA21`, `STRATUS_LAMBDA_IMAGE_NODEJS20_X`. Wins over the repository. |
| `STRATUS_LAMBDA_RIE_IMAGE` | The image the emulator is copied from for custom-base container images (default: the `provided.al2023` image). |

## How a function starts

- **Managed runtimes** (Python, Node.js, Java, Ruby, .NET): the zip is mounted
  read-only at `/var/task` and the image's own entrypoint is given the handler,
  exactly as Lambda does.
- **Custom runtimes** (`provided.*`): the emulator runs `/var/task/bootstrap`,
  or `/opt/bootstrap` from a layer. `_HANDLER` holds the handler string. A file
  named `bootstrap` is made executable on extraction, because zips built on
  Windows carry no Unix permissions.
- **Layers** are merged in order into one directory mounted at `/opt`, so each
  runtime finds them where Lambda puts them (`/opt/python`, `/opt/nodejs`,
  `/opt/java/lib`, …).

Each function also gets Lambda's standard variables (`AWS_LAMBDA_FUNCTION_NAME`,
`AWS_LAMBDA_FUNCTION_MEMORY_SIZE`, `AWS_LAMBDA_FUNCTION_TIMEOUT`, `AWS_REGION`,
…). The function's own `Environment` wins over them.

## Container-image functions

```sh
docker build -t orders-fn:latest .
aws lambda create-function --function-name orders \
  --package-type Image --code ImageUri=orders-fn:latest \
  --image-config Command=app.handler \
  --role arn:aws:iam::000000000000:role/lambda
```

`ImageUri` is any reference the local Docker daemon has; build or pull it
first. `ImageConfig` (`EntryPoint`, `Command`, `WorkingDirectory`) overrides the
image's own settings.

- Images built **on an AWS base image** run as built.
- Images built **on another base** usually start the runtime interface client
  directly (for example `python -m awslambdaric`). Stratus copies the emulator
  out of the `provided.al2023` image once, mounts it into the container and
  runs your entrypoint under it, as AWS documents for testing such images
  locally. Pre-pull `public.ecr.aws/lambda/provided:al2023` for this.

## Behaviour worth knowing

- **Warm containers.** A container stays warm for five minutes after its last
  invocation, so module-level state survives between invokes as on Lambda.
  The emulator runs one invocation at a time, so concurrent invocations of one
  function queue instead of scaling out.
- **Errors.** A raised exception, a runtime that exits, and a timeout all come
  back as `X-Amz-Function-Error: Unhandled` with Lambda's error document. After
  an error the emulator restarts the runtime, so module state resets; Lambda
  keeps it after a handled exception.
- **Timeouts** are enforced by the emulator from the function's `Timeout`. The
  container is replaced afterwards, as Lambda replaces the environment.
- **Logs.** `--log-type Tail` returns the invocation's output between the
  emulator's `START` and `REPORT` lines.
- **Architectures.** Containers run on the host's architecture. An `arm64`
  function on an x86 host needs Docker's emulation (binfmt/QEMU).
