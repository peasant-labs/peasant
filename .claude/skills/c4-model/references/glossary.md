# C4 glossary

Every term the C4 model uses, with its definition and its source. Quoted text is verbatim from
c4model.com as read on 2026-09-03. Unquoted text is a paraphrase or a note for this workspace.
When a definition matters in a review or a design argument, quote the source line, not this
file's paraphrase.

Source pages:

- Abstractions: https://c4model.com/abstractions and the per-term pages under it.
- Diagrams: https://c4model.com/diagrams and the per-diagram pages under it.
- Notation: https://c4model.com/diagrams/notation
- Checklist: https://c4model.com/diagrams/checklist
- Tooling: https://c4model.com/tooling
- FAQ: https://c4model.com/faq and https://c4model.com/diagrams/faq

## The abstractions

C4 defines a small, fixed set of things a diagram may contain. Every element on every diagram is
one of these. The set is a hierarchy: a software system is made of containers, a container is made
of components, a component is made of code elements. People sit outside the hierarchy and use
the systems.

### Person

A human user of the software system. The system context page lists the forms a person takes:
"People (e.g. users, actors, roles, or personas)". Model a person by role, not by name.
Examples in this workspace: the developer who records agent sessions, the reader who browses a
published transcript.

Source: https://c4model.com/abstractions and https://c4model.com/diagrams/system-context.
The dedicated page `/abstractions/person` returned 404 on 2026-09-03.

### Software system

"A software system is the highest level of abstraction and describes something that delivers
value to its users, whether they are human or not. This includes the software system you are
modelling, and the other software systems upon which your software system depends (or vice
versa)."

A useful test for the boundary of one software system: "something a single software development
team is building, owns, has responsibility for, and can see the internal implementation details
of."

The page also says what a software system is not: a product domain, a bounded context, a business
capability, a feature team, a tribe, or a squad. Those are ways to organise people, not software.

Source: https://c4model.com/abstractions/software-system

### Internal and external software system

The software system in scope is the one the diagram is about. Every other software system on the
diagram is a dependency of it, or depends on it. Draw it as external. External means outside
the scope and ownership of the diagram, not outside the company. In the ASCII notation the tag
is `[Software System, external]`.

Source: https://c4model.com/abstractions/software-system and
https://c4model.com/diagrams/system-context

### Container

"In the C4 model, a container represents an application or a data store." And: "A container is
something that needs to be running in order for the overall software system to work."

Examples the page lists: server-side web applications, client-side web applications, desktop
applications, mobile apps, console applications, serverless functions, databases, blob stores,
file systems, and shell scripts.

Not a Docker container. Simon Brown on the name: "I wanted a name that didn't imply anything
about the physical nature of how that container is executed." And: "it's unfortunate that
containerisation has become popular, because many software developers now associate the term
'container' with Docker."

Libraries are not containers: "A container is a runtime construct, like an application; whereas
Java JAR files, C# assemblies, DLLs, modules, etc are used to organise the code within those
applications."

Examples in this workspace: the peasant Go binary, the peasant web app, the peasant SQLite
store, the village backend, the village frontend, the village Postgres database, the village S3
bucket. Not containers: `schema`, `redact`, `fairtrade`. Those are modules imported by
containers.

Source: https://c4model.com/abstractions/container

### Component

"in the C4 model, a component is a grouping of related functionality encapsulated behind a
well-defined interface."

"With the C4 model, components are not separately deployable units. Instead, it's the container
that's the deployable unit."

What a component is made of depends on the language: classes and interfaces in object-oriented
languages, C files in a directory in C, modules with objects and functions in JavaScript,
modules with functions and types in functional languages.

Packages are not automatically components. The page says JAR files, assemblies, DLLs, packages,
namespaces, and folder structures are typically not components. One component can span several
packages, and one package can hold several components. The mapping is a design decision.

Examples in this workspace: inside the peasant backend container, `internal/ingest`,
`internal/transcript`, `internal/api`, and `internal/store` are each a component. A Go package
is a good default component boundary when its exported API is the interface.

Source: https://c4model.com/abstractions/component

### Code element

"Components are made up of one or more code elements constructed with the basic building blocks
of the programming language that you're using - classes, interfaces, enums, functions, objects,
etc."

Source: https://c4model.com/abstractions/code

### Relationship

A directed connection between two elements. On the diagram it is one arrow with one label. The
notation page fixes the rules: "Every line should represent a unidirectional relationship."
"Every line should be labelled, the label being consistent with the direction and intent of the
relationship (e.g. dependency or data flow). Try to be as specific as possible with the label,
ideally avoiding single words like, "Uses"." And: "Relationships between containers (typically
these represent inter-process communication) should have a technology/protocol explicitly
labelled."

The diagrams FAQ asks whether a line shows a dependency or a data flow. Either is allowed. Pick
one per diagram and say which in the key. In this workspace, default to dependency: the arrow
points from the element that makes the call to the element that receives it.

Source: https://c4model.com/diagrams/notation and https://c4model.com/diagrams/faq

### Element attributes: name, type, technology, description

Every element carries four attributes. The name is implicit. The notation page requires the
other three on the diagram:

- Name. Given.
- Type. "The type of every element should be explicitly specified (e.g. Person, Software System,
  Container or Component)."
- Technology. "Every container and component should have a technology explicitly specified."
- Description. "Every element should have a short description, to provide an 'at a glance' view
  of key responsibilities."

Source: https://c4model.com/diagrams/notation

### Boundary and group

A boundary is the drawn edge of a parent element when the diagram zooms into it: the software
system boundary on a container diagram, the container boundary on a component diagram, the
deployment node boundary on a deployment diagram. A group is an optional grouping of elements
that share something, such as a team or a department, on any diagram except Code. Groups carry no meaning in
the model beyond the label. In the ASCII notation both are double-line boxes.

Source: https://c4model.com/diagrams/container, https://c4model.com/diagrams/component,
https://c4model.com/diagrams/deployment

## The diagrams

C4 has four core diagrams, one per level, and three supplementary diagrams. The core four are
static structure diagrams. The diagrams page: "you don't need to use all 4 levels of diagram;
only those that add value - the system context and container diagrams are sufficient for most
software development teams."

Each diagram fixes its scope, its primary elements, its supporting elements, and its audience.

### Level 1: System Context diagram

- Scope: "A single software system."
- Primary elements: the software system in scope.
- Supporting elements: "People (e.g. users, actors, roles, or personas) and software systems
  (external dependencies) that are directly connected to the software system in scope."
- Audience: "Everybody, both technical and non-technical people, inside and outside the
  software development team."
- Recommended for all teams: yes.

Source: https://c4model.com/diagrams/system-context

### Level 2: Container diagram

- Scope: "A single software system."
- Primary elements: the containers inside the software system in scope.
- Supporting elements: "People and software systems directly connected to the containers."
- Audience: software architects, developers, and operations staff.
- Recommended for all teams: yes.
- Says nothing about deployment. The page excludes clustering, load balancers, and failover on
  purpose: "Deployment information is better captured via one or more deployment diagrams, one
  per environment."

Source: https://c4model.com/diagrams/container

### Level 3: Component diagram

- Scope: "A single container."
- Primary elements: the components inside the container in scope.
- Supporting elements: "Containers (within the software system in scope) plus people and
  software systems directly connected to the components."
- Audience: "Software architects and developers."
- Recommended for all teams: no. "only create component diagrams if you feel they add value,
  and consider automating their creation for long-lived documentation."

Source: https://c4model.com/diagrams/component

### Level 4: Code diagram

- Scope: one component.
- Elements: code elements, usually as a UML class diagram or an entity relationship diagram.
- Audience: "Software architects and developers."
- Recommended for all teams: no. "This level of detail is not recommended for anything but the
  most important or complex components." And: "Ideally this diagram would be automatically
  generated using tooling (e.g. an IDE or UML modelling tool)."
- The diagrams FAQ on keeping them current: for code-level diagrams "the recommendation is to
  (1) not create them at all or (2) generate them on-demand using tooling such as your IDE".

Source: https://c4model.com/diagrams/code and https://c4model.com/diagrams/faq

### System Landscape diagram (supplementary)

- Scope: "An enterprise/organisation/department/etc."
- Primary elements: "People and software systems related to the chosen scope."
- Audience: "Technical and non-technical people, inside and outside the software development
  team."
- Purpose: a system context diagram with no single system in focus. It maps a portfolio. For a
  polyrepo workspace this is the diagram that shows every repo that ships a runnable system and
  how they connect.

Source: https://c4model.com/diagrams/system-landscape

### Dynamic diagram (supplementary)

- Scope: "A particular feature, story, use case, etc."
- Elements: "Your choice - you can show software systems, containers, or components interacting
  at runtime."
- Audience: "Technical and non-technical people, inside and outside the software development
  team."
- Form: based on "a UML communication diagram (previously known as a 'UML collaboration
  diagram')". Elements sit where they sit on the static diagram, and numbered labels give the
  order of the interactions. It is not a sequence diagram with a strict vertical timeline.
- Use only for an interesting, recurring, or intricate interaction. Do not draw one per feature.

Source: https://c4model.com/diagrams/dynamic

### Deployment diagram (supplementary)

- Scope: "One or more software systems within a single deployment environment (e.g. production,
  staging, development, etc)."
- Primary elements: "Deployment nodes, software system instances, and container instances."
- Supporting elements: "Infrastructure nodes used in the deployment of the software system."
- Audience: "Technical people inside and outside of the software development team; including
  software architects, developers, infrastructure architects, and operations/support staff."
- One diagram per environment. This is the only diagram where Docker, Kubernetes, Railway,
  and the developer's laptop appear.

Source: https://c4model.com/diagrams/deployment

## Deployment vocabulary

### Deployment environment

One named place where the system runs: production, staging, development, a developer's machine.
Each environment gets its own deployment diagram.

Source: https://c4model.com/diagrams/deployment

### Deployment node

"A deployment node represents where an instance of a software system/container is running;
perhaps physical infrastructure (e.g. a physical server or device), virtualised infrastructure
(e.g. IaaS, PaaS, a virtual machine), containerised infrastructure (e.g. a Docker container), an
execution environment (e.g. a database server, Java EE web/application server, Microsoft IIS),
etc."

Deployment nodes nest: a Railway project holds a service, a service holds a Docker container,
the Docker container holds the Node runtime, the runtime holds the container instance.

Source: https://c4model.com/diagrams/deployment

### Infrastructure node

A piece of infrastructure that takes part in the deployment but runs none of the system's own
containers. The page lists "infrastructure nodes such as DNS services, load balancers,
firewalls, etc."

Source: https://c4model.com/diagrams/deployment

### Software system instance and container instance

One running copy of a software system or a container, inside a deployment node. The container
diagram shows the container once. The deployment diagram shows one container instance per place
it runs, so a backend with three replicas appears three times, or once with a count.

Source: https://c4model.com/diagrams/deployment

## Notation vocabulary

### Notation independent, tooling independent

C4 fixes the abstractions and the diagram types, not the drawing. "The C4 model is notation
independent, and doesn't prescribe any particular notation." The home page adds that it is
tooling independent. Any notation works if it obeys the rules on the notation page. This skill's
ASCII notation is one such notation.

Source: https://c4model.com/ and https://c4model.com/diagrams/notation

### Title

"Every diagram should have a title describing the diagram type and scope (e.g. "System Context
diagram for My Software System")." The ASCII notation form is `<Type> diagram: <scope>`.

Source: https://c4model.com/diagrams/notation

### Key, legend

"Every diagram should have a key/legend explaining the notation being used (e.g. shapes,
colours, border styles, line types, arrow heads, etc)." The key explains
the shapes, line styles, and colours the diagram uses. It does not restate the element
descriptions.

Source: https://c4model.com/diagrams/notation

### Acronyms and abbreviations

They "should be understandable by all audiences, or explained in the diagram key/legend". Expand
or define every acronym
the intended audience may not know. `WS` is not enough; write `WebSocket`.

Source: https://c4model.com/diagrams/notation

### Colour, shape, icon, line style

Not prescribed. Use them if they help, keep them consistent, explain them in the key, and do not
depend on colour alone. ASCII has no colour, so the ASCII notation carries type in the tag and
uses only two box styles.

Source: https://c4model.com/diagrams/notation and https://c4model.com/diagrams/checklist

## Modelling vocabulary

### Model, view, diagram

A model is "a single definition of all elements and the relationships between them". A view is
a selection of the model at one scope and one level. A diagram is a rendered view. Two diagrams
drawn from one model cannot disagree about an element's name, type, or technology.

Source: https://c4model.com/tooling

### Diagramming tool, modelling tool

The tooling page separates the two. "The domain language of diagramming tools is 'boxes and
lines'", so nothing can be validated or queried, and "if you rename a box, you need to rename it
across every diagram where it appears". A modelling tool builds "a non-visual model of your
software architecture" and then creates "different views (that become diagrams) on top of that
model". Mermaid, PlantUML, D2, and this skill's ASCII are diagramming. Structurizr and LikeC4 are
modelling. The element and relationship tables in the workflow are the smallest possible model.

Source: https://c4model.com/tooling

### Diagrams as code

Diagrams kept as text in version control and rendered by a tool. Diagramming tools do this per
diagram. Modelling tools do it per model. The ASCII notation needs no renderer, which is its
reason to exist.

Source: https://c4model.com/tooling

### Workspace

Structurizr's name for one model plus its views plus its documentation, in one DSL file or one
JSON document. Not a C4 term, but the word appears wherever Structurizr does. Do not confuse it
with the polyrepo workspace.

Source: https://docs.structurizr.com/dsl

### Model-code gap

The distance between what the architecture documents say and what the code does. The C4 FAQ
names closing it as one of the model's aims, describing C4 as a way to "reduce the gap between
architecture models and actual code". The term is from George Fairbanks, Just Enough Software
Architecture, 2010.

Source: https://c4model.com/faq

### Zoom level, map analogy

The introduction describes C4 as creating "maps of your code, at various levels of detail, in
the same way you would use something like Google Maps to zoom in and out of an area you are
interested in." Level 1 is the country map. Level 4 is the street.

Source: https://c4model.com/introduction

### Level

The four numbered levels are Context (1), Containers (2), Components (3), and Code (4). The
supplementary diagrams have no level number.

Source: https://c4model.com/diagrams

## What C4 does not cover

The FAQ answers "Why doesn't the C4 model cover business processes, workflows, state machines,
domain models, data models, etc?" by saying C4 is for the static structure of a software system.
Use another notation for those. On libraries: "The C4 model is really designed to model a
software system, at various levels of abstraction. To document a library, framework or SDK, you
might be better off using something like UML." The alternative the FAQ offers is a usage example
that shows the consumer's own components next to the ones the library provides.

Source: https://c4model.com/faq
