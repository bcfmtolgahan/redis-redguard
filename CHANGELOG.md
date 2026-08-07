# Changelog

All notable changes to Redguard will be documented in this file.

## [0.2.0] - 2026-01-09

### Added

#### Security Features
- **RedisUser CRD**: Fine-grained ACL management for Redis 6+
  - Category-based permissions (@read, @write, @dangerous, etc.)
  - Command-level access control
  - Key pattern restrictions
  - Pub/Sub channel restrictions
  - Password management via Kubernetes Secrets

- **TLS/SSL Support**: Encrypted Redis connections
  - Certificate-based encryption
  - Mutual TLS support
  - CA certificate validation
  - Integration with Kubernetes Secrets

#### Backup & Recovery
- **RedisBackup CRD**: S3-based backup management
  - Scheduled backups using cron expressions
  - S3-compatible storage support (AWS S3, MinIO, etc.)
  - Retention policies with automatic cleanup
  - Gzip compression
  - IAM role support (IRSA on EKS)
  - One-time and scheduled backups

#### Documentation
- Comprehensive FEATURES.md with examples
- Sample CRs for all new features
- Migration and troubleshooting guides
- Best practices documentation

### Changed
- Enhanced README with security and backup features
- Updated CRD schemas with additional validations

## [0.1.0] - 2026-01-09

### Added
- Initial release
- RedisSentinel CRD for high-availability Redis clusters
- Automatic failover with Redis Sentinel
- StatefulSet-based deployment
- Persistent storage support
- Resource management
- Basic authentication support
- Helm chart for operator deployment
- Comprehensive testing suite
- Failover testing automation

### Features
- 3-node Redis cluster with Sentinel
- Automatic master election
- Persistent volume claims
- ConfigMap-based configuration
- Health checks and probes
- Status reporting
- kubectl integration
