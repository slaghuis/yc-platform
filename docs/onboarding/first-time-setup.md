git clone https://github.com/<org>/yc-platform.git
cd yc-platform

task bootstrap        # installs hooks, creates .env, generates dev keys
task devstack:up      # starts Postgres, NATS, MinIO, Jaeger, MailHog
task test             # verify everything compiles and tests pass