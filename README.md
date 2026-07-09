# [Project Name]

Project description goes here.

## Setup Guidelines

This template supports two different project setups. Choose the one that fits your needs:

### Single Project Setup

For standalone applications or single-purpose projects:

- Use **[src/](src/)** for your main application code
- The **[packages/](packages/)** and **[projects/](projects/)** directories are **not needed** and can be removed
- All documentation directories ([docs/](docs/)) remain relevant
- Keep [scripts/](scripts/) and [test/](test/) for development workflows

### Monorepo Setup

For managing multiple related projects or shared packages:

- Use **[packages/](packages/)** for shared libraries and reusable modules
- Use **[projects/](projects/)** for individual applications or sub-projects
- Each package/project can have its own [src/](src/), tests, and configuration
- The root **[src/](src/)** directory may not be needed in this setup
- All documentation and development directories remain relevant

## Directory Structure

This project follows a structured organization to maintain clarity and separation of concerns:

### Source Code

- **[src/](src/)** - Main source code directory containing the application implementation and unit tests (unit tests are recommended to be co-located with the source code)

### Packages & Dependencies

- **[packages/](packages/)** - Internal packages and reusable modules
- **[submodule/github.com/](submodule/github.com/)** - External GitHub submodules and third-party dependencies (can be removed if not needed)

### Documentation

- **[docs/goals/](docs/goals/)** - Project goals, objectives, and success metrics
- **[docs/prd/](docs/prd/)** - Product Requirements Documents (PRDs) defining features and functionality
- **[docs/specs/](docs/specs/)** - Technical specifications and detailed design documents
- **[docs/plans/](docs/plans/)** - Implementation plans and project roadmaps
- **[docs/analyst/](docs/analyst/)** - Business analysis, requirements analysis, and data insights
- **[docs/architect/](docs/architect/)** - Architecture designs, system diagrams, and technical decisions
- **[docs/designer/](docs/designer/)** - UI/UX designs, mockups, and design specifications for frontend projects (can be removed if not needed)

### Development

- **[scripts/](scripts/)** - Utility scripts for build, deployment, and automation tasks
- **[test/](test/)** - Test files including integration tests, E2E tests, and test utilities. Unit tests are not included

### Projects

- **[projects/](projects/)** - Sub-projects, standalone modules, or related project components
