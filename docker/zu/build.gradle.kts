import com.github.jengelman.gradle.plugins.shadow.tasks.ShadowJar
import org.jetbrains.kotlin.gradle.tasks.KotlinCompile
import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    kotlin("jvm") version "2.4.21"
    id("com.gradleup.shadow") version "9.6.1"
}

repositories {
    mavenCentral()
}

val javaVersion = JavaVersion.toVersion(
    providers.gradleProperty("javaVersion").orElse(providers.provider {
        file("../zookeeper-image/Dockerfile").readLines()
            .single { it.startsWith("ARG java_version=") }
            .substringAfter("=")
    }).get()
)

java {
    sourceCompatibility = javaVersion
    targetCompatibility = javaVersion
}

val zookeeperVersion = "3.9.7"

dependencies {
    implementation(kotlin("stdlib"))
    implementation("org.apache.zookeeper:zookeeper:$zookeeperVersion")
    runtimeOnly("ch.qos.logback:logback-classic:1.6.6")
}

tasks.withType<ShadowJar>() {
    archiveClassifier.set("")
    filesMatching("META-INF/*.kotlin_module") {
        duplicatesStrategy = DuplicatesStrategy.INCLUDE
    }
    manifest {
        attributes["Main-Class"] = "io.pravega.zookeeper.MainKt"
    }
}

tasks.withType<KotlinCompile> {
  compilerOptions {
    jvmTarget.set(JvmTarget.fromTarget(javaVersion.majorVersion))
  }
}
