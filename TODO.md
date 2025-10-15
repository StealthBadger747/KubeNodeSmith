# Kubernetes Autoscaler TODO

This document tracks all planned improvements and features for the Kubernetes autoscaler project.

## 🚀 High Priority Quick List

**Immediate Action Items:**
- [ ] **Build a reconciliation loop to detect drifts** - Detect drifts.
- [ ] **Convert configuration into CRDs** - Make autoscaler Kubernetes-native
- [ ] **Implement NodePool targeting** - Target specific nodepools for scaling
- [ ] **Fix pool limits calculations** - Include `vmMemOverheadMiB` in resource calculations
- [ ] **Add ephemeral disks support** - Remove hardcoded `local-zfs:16,serial=CONTAINERD01,discard=on,ssd=1`, make configurable
- [ ] **OTEL** - Implement OTEL tracing
- [ ] **Implement scale up/down policies** - `batchSize`, `stabilizationWindow`, `maxConcurrent`, `drainTimeout`
- [ ] **Add Redis/Asynq job queue** - Background job processing for node operations
- [ ] **Implement lease acquisition** - Prevent concurrent scaling operations
- [ ] **Fix VMID exhaustion** - Better VMID management to prevent infinite loops
- [ ] **Add labels/annotations config** - Karpenter-like node metadata support

## Detailed Feature Roadmap

### 1. NodePool Targeting System
- [ ] **Implement way to target specific nodepools**
  - Add node pool selection logic in the scaler
  - Support filtering pods by node pool affinity/requirements
  - Add configuration for pod-to-nodepool mapping rules
  - Implement node pool priority/weighting system

### 2. Resource Calculation Improvements
- [ ] **Rework pool limits calculations to include `vmMemOverheadMiB`**
  - Update `checkPoolLimits()` function to account for VM memory overhead
  - Modify `GetPoolResourceUsage()` to include overhead in calculations
  - Add overhead tracking in `PoolResourceUsage` struct
  - Update resource validation logic to prevent over-provisioning

### 3. Storage and Disk Management
- [ ] **Add ephemeral disks support**
  - Remove hardcoded `local-zfs:16,serial=CONTAINERD01,discard=on,ssd=1` in Proxmox provider
  - Add configurable ephemeral disk options to `ProxmoxProviderOptions`
  - Support multiple disk types (local-zfs, local-lvm, etc.)
  - Add disk size configuration per node pool
  - Implement disk cleanup on node deprovisioning

### 4. Job Queue and Task Management
- [ ] **Add Redis/Tasks/jobqueue using asynq**
  - Integrate asynq for background job processing
  - Implement job queue for node provisioning/deprovisioning
  - Add retry logic and job failure handling
  - Create job status tracking and monitoring
  - Add Redis configuration to provider options

### 5. Configuration Enhancements
- [ ] **Add config for labels/annotations (like Karpenter)**
  - Extend `MachineTemplate` to support annotations
  - Add taint support for node pools
  - Implement startup taints (like Karpenter's `karpenter.sh/unregistered`)
  - Add node class concept similar to Karpenter's NodeClass
  - Support custom node metadata and tags

### 6. CRD Implementation
- [ ] **Convert configuration into Custom Resource Definitions (CRDs)**
  - Create `NodePool` CRD for node pool management
  - Create `Provider` CRD for provider configuration
  - Create `MachineTemplate` CRD for machine specifications
  - Implement CRD validation and admission webhooks
  - Add CRD-based configuration management
  - Create migration tool from YAML config to CRDs
  - Add CRD status subresources for real-time status
  - Implement CRD controllers for reconciliation

## Karpenter-like features & NodeClaims

### 7. NodeClaims Concept Exploration
- [ ] **Explore and implement NodeClaims concepts**
  - Study [Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/)
  - Implement NodeClaim-like resource for tracking node lifecycle
  - Add node state management (Launched, Registered, Initialized, Ready)
  - Implement node claim reconciliation loop
  - Add node claim status conditions and events
  - Create node claim cleanup and finalizers

### 8. Advanced Scheduling Features
- [ ] **Implement Karpenter-inspired scheduling**
  - Add node requirements and constraints matching
  - Implement node consolidation logic
  - Add disruption policies and controls
  - Support node expiration and termination grace periods
  - Add node capacity type support (spot, on-demand, etc.)

## Scale Up/Down Policy Implementation

### 9. Scale Up Policies
- [ ] **Implement scale up batch processing**
  - Add `batchSize` support for concurrent node creation
  - Implement `stabilizationWindow` to prevent rapid scaling
  - Add scale up rate limiting and throttling
  - Create scale up decision batching logic

### 10. Scale Down Policies
- [ ] **Implement scale down controls**
  - Add `maxConcurrent` support for node deletion
  - Implement `drainTimeout` for pod eviction
  - Add graceful node draining before deletion
  - Create scale down candidate evaluation logic
  - Implement pod disruption budget (PDB) awareness

## Infrastructure and Reliability

### 11. Error Handling and Resilience
- [ ] **Improve error handling throughout codebase**
  - Replace panic() calls with proper error handling
  - Add comprehensive error logging and metrics
  - Implement circuit breaker pattern for provider calls
  - Add retry logic with exponential backoff
  - Create error recovery mechanisms

### 12. Monitoring and Observability
- [ ] **Add comprehensive monitoring**
  - Implement Prometheus metrics export
  - Add structured logging with correlation IDs
  - Create health check endpoints for all components
  - Add tracing support for request flow
  - Implement alerting for critical failures

### 13. Configuration Management
- [ ] **Enhance configuration system**
  - Add configuration validation and schema checking
  - Implement configuration hot-reloading
  - Add configuration versioning and migration
  - Create configuration templates and examples
  - Add environment-specific configuration support

## Provider Enhancements

### 14. Proxmox Provider Improvements
- [ ] **Enhance Proxmox integration**
  - Improve node selection algorithm (currently random)
  - Add resource utilization-based node selection
  - Implement node health checking and monitoring
  - Add support for Proxmox resource pools
  - Create VM template management

### 15. Multi-Provider Support
- [ ] **Expand provider ecosystem**
  - Add VMware vSphere provider
  - Implement OpenStack provider
  - Add bare metal provider (IPMI/Redfish)
  - Create provider abstraction layer
  - Add provider-specific configuration validation

## Testing and Quality

### 16. Testing Infrastructure
- [ ] **Add comprehensive test suite**
  - Unit tests for all core functions
  - Integration tests with mock providers
  - End-to-end tests with real Kubernetes clusters
  - Performance and load testing
  - Chaos engineering tests

### 17. Documentation
- [ ] **Improve project documentation**
  - Add comprehensive README with examples
  - Create architecture documentation
  - Add API documentation
  - Create troubleshooting guides
  - Add deployment and configuration guides

## Performance and Scalability

### 18. Performance Optimizations
- [ ] **Optimize for large-scale deployments**
  - Implement efficient pod/node listing with informers
  - Add caching for frequently accessed data
  - Optimize resource calculation algorithms
  - Implement connection pooling for provider APIs
  - Add batch processing for bulk operations

### 19. Resource Management
- [ ] **Improve resource tracking**
  - Add real-time resource usage monitoring
  - Implement resource prediction algorithms
  - Add capacity planning features
  - Create resource utilization dashboards
  - Implement resource optimization suggestions

## Security and Compliance

### 20. Security Enhancements
- [ ] **Strengthen security posture**
  - Add RBAC configuration and validation
  - Implement secure credential management
  - Add network security policies
  - Create security scanning and compliance checks
  - Implement audit logging

### 21. Multi-tenancy Support
- [ ] **Add multi-tenant capabilities**
  - Implement namespace-based isolation
  - Add tenant-specific node pools
  - Create resource quota enforcement
  - Add tenant-specific configuration
  - Implement cross-tenant resource sharing controls

## Operational Features

### 22. Operational Tools
- [ ] **Add operational utilities**
  - Create CLI tool for manual operations
  - Add node pool management commands
  - Implement configuration validation tools
  - Create debugging and diagnostic tools
  - Add maintenance mode support

### 23. Backup and Recovery
- [ ] **Implement backup and recovery**
  - Add configuration backup functionality
  - Create disaster recovery procedures
  - Implement state persistence
  - Add configuration migration tools
  - Create rollback mechanisms


## Notes

- This TODO list is prioritized by impact and implementation complexity
- Items marked as "High Priority" should be addressed first
- Consider implementing features incrementally with proper testing
- Regular review and updates of this TODO list is recommended
- Some features may require breaking changes to the current API

## References

- [Karpenter NodeClaims Documentation](https://karpenter.sh/docs/concepts/nodeclaims/)
- [Karpenter Concepts](https://karpenter.sh/docs/concepts/)
- [Kubernetes Cluster Autoscaler](https://github.com/kubernetes/autoscaler)
