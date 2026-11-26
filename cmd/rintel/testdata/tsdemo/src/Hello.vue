<template>
  <p>{{ greeting }}</p>
</template>

<script setup lang="ts">
import { describe } from "./service";

const greeting = describe({ id: 1, name: "ann" });
</script>
